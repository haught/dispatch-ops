// Package runner is the program inside every execution environment image.
// It fetches its job from the API, prepares the workspace, runs the command,
// streams the output back, and reports the result.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/runner/runnerapi"
)

// Config is how the task manager tells the runner what to do, through
// environment variables on the job pod.
type Config struct {
	APIURL    string    // DISPATCH_API_URL
	JobID     uuid.UUID // DISPATCH_JOB_ID
	Token     string    // read from DISPATCH_JOB_TOKEN_FILE
	WorkDir   string    // DISPATCH_WORKDIR
	Version   string
	Stdout    io.Writer // the pod log
	Heartbeat time.Duration
	// KillGrace is how long the command gets between SIGTERM and SIGKILL.
	KillGrace time.Duration
	// Askpass is a program git runs to ask for a username or password: the
	// runner binary itself (see Askpass). Empty means git can't ask, so
	// cloning over HTTP(S) works only without a password.
	Askpass string
}

// ConfigFromEnv reads the runner's configuration.
func ConfigFromEnv(version string) (Config, error) {
	c := Config{
		APIURL:    strings.TrimRight(os.Getenv("DISPATCH_API_URL"), "/"),
		WorkDir:   os.Getenv("DISPATCH_WORKDIR"),
		Version:   version,
		Stdout:    os.Stdout,
		Heartbeat: 10 * time.Second,
		KillGrace: 10 * time.Second,
	}
	var errs []error
	if c.APIURL == "" {
		errs = append(errs, errors.New("DISPATCH_API_URL is not set"))
	}
	id, err := uuid.Parse(os.Getenv("DISPATCH_JOB_ID"))
	if err != nil {
		errs = append(errs, fmt.Errorf("DISPATCH_JOB_ID is not a job ID: %q", os.Getenv("DISPATCH_JOB_ID")))
	}
	c.JobID = id
	tokenFile := os.Getenv("DISPATCH_JOB_TOKEN_FILE")
	if tokenFile == "" {
		errs = append(errs, errors.New("DISPATCH_JOB_TOKEN_FILE is not set"))
	} else if b, err := os.ReadFile(tokenFile); err != nil {
		errs = append(errs, fmt.Errorf("read job token: %w", err))
	} else {
		c.Token = strings.TrimSpace(string(b))
	}
	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}
	if exe, err := os.Executable(); err == nil {
		c.Askpass = exe
	}
	return c, errors.Join(errs...)
}

// Run runs the job. It returns an error only if the outcome could not be
// reported; a failed command is a successfully reported failure.
//
// Canceling ctx (the pod is being terminated) stops the command.
func Run(ctx context.Context, cfg Config) error {
	client, err := newClient(cfg.APIURL, cfg.Token, cfg.Version)
	if err != nil {
		return err
	}
	log := func(format string, args ...any) {
		fmt.Fprintf(cfg.Stdout, "[dispatch-runner] "+format+"\n", args...)
	}
	log("dispatch-runner %s (protocol %d), job %s", cfg.Version, runnerapi.ProtocolVersion, cfg.JobID)

	spec, err := fetchSpec(ctx, client, cfg)
	if err != nil {
		return fmt.Errorf("fetch job spec: %w", err)
	}

	stop := newStopper()
	ev := newEmitter(client, cfg.JobID, cfg.Stdout,
		func() { stop.trigger(stopCancel) },
		func() { stop.trigger(stopJobOver) })

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	go heartbeat(hbCtx, client, cfg, stop)

	result := execute(ctx, cfg, spec, ev, stop)
	stopHeartbeat()

	sendErr := ev.close()
	if errors.Is(sendErr, errJobOver) || stop.reason() == stopJobOver {
		log("the server ended this job; not reporting a result")
		return nil
	}
	if sendErr != nil {
		log("could not send all output: %v", sendErr)
	}

	finishCtx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		// The pod is being terminated: report quickly or not at all.
		var cancel context.CancelFunc
		finishCtx, cancel = context.WithTimeout(finishCtx, 5*time.Second)
		defer cancel()
	}
	err = retry(finishCtx, sendRetryTime, func(ctx context.Context) error {
		resp, err := client.FinishRunnerJobWithResponse(ctx, cfg.JobID, result)
		if err != nil {
			return err
		}
		return classify(resp.StatusCode(), resp.Body, 204)
	})
	if errors.Is(err, errJobOver) {
		log("the server ended this job before the result was reported")
		return nil
	}
	if err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	log("reported status %s", result.Status)
	return nil
}

func fetchSpec(ctx context.Context, client *runnerapi.ClientWithResponses, cfg Config) (*runnerapi.RunnerJobSpec, error) {
	var spec *runnerapi.RunnerJobSpec
	err := retry(ctx, 2*time.Minute, func(ctx context.Context) error {
		resp, err := client.GetRunnerJobSpecWithResponse(ctx, cfg.JobID, &runnerapi.GetRunnerJobSpecParams{
			Protocol: runnerapi.ProtocolVersion, Version: cfg.Version,
		})
		if err != nil {
			return err
		}
		if err := classify(resp.StatusCode(), resp.Body, 200); err != nil {
			return err
		}
		spec = resp.JSON200
		return nil
	})
	return spec, err
}

// heartbeat tells the API the job is alive, and learns about cancellation.
func heartbeat(ctx context.Context, client *runnerapi.ClientWithResponses, cfg Config, stop *stopper) {
	t := time.NewTicker(cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		resp, err := client.RunnerHeartbeatWithResponse(ctx, cfg.JobID)
		if err != nil {
			continue // transient; the task manager tolerates missed beats
		}
		switch err := classify(resp.StatusCode(), resp.Body, 200); {
		case errors.Is(err, errJobOver):
			stop.trigger(stopJobOver)
			return
		case err == nil && resp.JSON200 != nil && resp.JSON200.CancelRequested:
			stop.trigger(stopCancel)
		}
	}
}

// --- Stopping ----------------------------------------------------------------

type stopReason int

const (
	stopNone stopReason = iota
	stopCancel
	stopTimeout
	stopJobOver
	stopTerminated
)

// stopper records the first reason to stop the command.
type stopper struct {
	ch    chan struct{}
	first chan stopReason
}

func newStopper() *stopper {
	return &stopper{ch: make(chan struct{}), first: make(chan stopReason, 1)}
}

func (s *stopper) trigger(r stopReason) {
	select {
	case s.first <- r:
		close(s.ch)
	default:
	}
}

func (s *stopper) reason() stopReason {
	select {
	case r := <-s.first:
		s.first <- r
		return r
	default:
		return stopNone
	}
}

// --- Execution ---------------------------------------------------------------

// execute prepares the workspace, checks out the project, runs the command
// and returns the result to report.
func execute(ctx context.Context, cfg Config, spec *runnerapi.RunnerJobSpec, ev *emitter, stop *stopper) runnerapi.RunnerFinish {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	result := runnerapi.RunnerFinish{}
	fail := func(reason string) runnerapi.RunnerFinish {
		return withReason(result, runnerapi.Error, reason, ev)
	}

	// The timeout covers the checkout too. stopCtx ends when anything stops
	// the job, which aborts a checkout in progress.
	done := make(chan struct{})
	defer close(done)
	stopCtx, cancelStop := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelStop()
	go func() {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-t.C:
			stop.trigger(stopTimeout)
		case <-ctx.Done():
			stop.trigger(stopTerminated)
		case <-stop.ch:
		case <-done:
			return
		}
		cancelStop()
	}()

	ws, err := prepare(cfg, spec)
	if err != nil {
		return fail("Could not prepare the workspace: " + err.Error())
	}
	if spec.Project != nil {
		rev, err := checkout(stopCtx, cfg, ws, spec.Project, ev)
		if rev != "" {
			result.ScmRevision = &rev
		}
		if r := stop.reason(); r != stopNone {
			return stopped(result, r, timeout, ev)
		}
		if err != nil {
			return fail("Could not check out the project: " + err.Error())
		}
		if spec.JobType == runnerapi.Ansible {
			out, flush := ev.lineWriter()
			err := installRequirements(stopCtx, cfg, ws, spec.Project, spec.Verbosity, ev.runner, out)
			flush()
			if r := stop.reason(); r != stopNone {
				return stopped(result, r, timeout, ev)
			}
			if err != nil {
				return fail("Could not install the project's requirements: " + err.Error())
			}
		}
	}
	c, err := buildCommand(spec, ws)
	if err != nil {
		return fail(err.Error())
	}

	cmd := exec.Command(c.argv[0], c.argv[1:]...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	// Its own process group, so the whole tree can be signaled.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail("Could not create the output pipe: " + err.Error())
	}
	cmd.Stdout, cmd.Stderr = pw, pw // one pipe keeps stdout and stderr in order

	if r := stop.reason(); r != stopNone {
		pw.Close()
		pr.Close()
		return stopped(result, r, timeout, ev)
	}
	ev.runner(fmt.Sprintf("Running %s (timeout %s)", quoteCommand(c.argv), timeout))
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fail("Could not start the command: " + err.Error())
	}
	pw.Close() // the child has its copy
	consumed := make(chan struct{})
	go func() {
		ev.consume(pr, c.eventMarker)
		close(consumed)
	}()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var waitErr error
	reason := stopNone
	select {
	case waitErr = <-exited:
		// It finished on its own; a stop request that raced with that is moot.
	case <-stop.ch:
		reason = stop.reason()
		switch reason {
		case stopCancel:
			ev.runner("Cancel requested; stopping the command")
		case stopTimeout:
			ev.runner(fmt.Sprintf("Timed out after %s; stopping the command", timeout))
		case stopTerminated:
			ev.runner("The job pod is shutting down; stopping the command")
		}
		waitErr = terminate(cmd, exited, cfg.KillGrace)
	}
	// Leftover background processes would hold the output pipe open.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = pr.SetReadDeadline(time.Now().Add(5 * time.Second))
	<-consumed
	pr.Close()

	exitCode := cmd.ProcessState.ExitCode()
	if exitCode >= 0 {
		result.ExitCode = &exitCode
	}
	if reason != stopNone {
		return stopped(result, reason, timeout, ev)
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		ev.runner("The command finished successfully")
		result.Status = runnerapi.Successful
		return result
	case errors.As(waitErr, &exitErr) && exitCode >= 0:
		return withReason(result, runnerapi.Failed, fmt.Sprintf("The command exited with code %d", exitCode), ev)
	case errors.As(waitErr, &exitErr):
		return withReason(result, runnerapi.Failed, "The command was killed: "+exitErr.String(), ev)
	default:
		return withReason(result, runnerapi.Error, "Waiting for the command failed: "+waitErr.Error(), ev)
	}
}

// stopped is the result of a job stopped for reason.
func stopped(r runnerapi.RunnerFinish, reason stopReason, timeout time.Duration, ev *emitter) runnerapi.RunnerFinish {
	switch reason {
	case stopCancel:
		return withReason(r, runnerapi.Canceled, "Canceled", ev)
	case stopTimeout:
		return withReason(r, runnerapi.Failed, fmt.Sprintf("Timed out after %s", timeout), ev)
	case stopTerminated:
		return withReason(r, runnerapi.Error, "The job pod was terminated", ev)
	default:
		return withReason(r, runnerapi.Error, "Stopped: the server ended the job", ev)
	}
}

func withReason(r runnerapi.RunnerFinish, status runnerapi.RunnerFinishStatus, reason string, ev *emitter) runnerapi.RunnerFinish {
	ev.runner(reason)
	r.Status = status
	r.FailureReason = &reason
	return r
}

// terminate sends SIGTERM to the command's process group, then SIGKILL if
// it hasn't exited after grace.
func terminate(cmd *exec.Cmd, exited <-chan error, grace time.Duration) error {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-exited:
		return err
	case <-time.After(grace):
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return <-exited
}

func quoteCommand(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	s := strings.Join(q, " ")
	// Keep the log message to one readable line; the job page shows the full command.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "") + " …"
	}
	return s
}

// --- Workspace ---------------------------------------------------------------

// workspace is where a job's files go.
type workspace struct {
	dir     string // the working directory
	secrets string // memory-backed in job pods; credentials go here
	// env is the command's environment, pointing at the files below.
	env        []string
	inventory  string // the inventory file, or "" if the job has none
	vars       string // the extra vars file
	projectDir string // where the project is checked out, or "" if none
	// ansibleEnv is extra environment for ansible-playbook: the search
	// paths of the roles and collections installed from the project's
	// requirements files.
	ansibleEnv []string
}

// path returns a path inside the workspace's private directory, which the
// runner uses for its own files.
func (w *workspace) path(elem ...string) string {
	return filepath.Join(append([]string{w.dir, ".dispatch"}, elem...)...)
}

// secret returns a path for runner-written secrets.
func (w *workspace) secret(elem ...string) string {
	return filepath.Join(append([]string{w.secrets, "runner"}, elem...)...)
}

// prepare writes the job's inputs to files and sets up the command's
// environment, which points at them:
//
//	DISPATCH_JOB_ID          the job ID
//	DISPATCH_WORKDIR         the working directory
//	DISPATCH_PROJECT_DIR     the project checkout (ansible and script jobs)
//	DISPATCH_INVENTORY       Ansible JSON inventory (only if the job has one)
//	DISPATCH_VARS            extra vars, as a JSON object
//	DISPATCH_LIMIT           host pattern limit (may be empty)
//	DISPATCH_CREDENTIALS     all credentials as JSON: [{name, kind, inputs}]
//	DISPATCH_CREDENTIALS_DIR one directory per credential, one file per input
func prepare(cfg Config, spec *runnerapi.RunnerJobSpec) (*workspace, error) {
	ws := &workspace{dir: cfg.WorkDir, secrets: filepath.Join(cfg.WorkDir, "secrets")}
	for _, d := range []string{ws.dir, ws.secrets, ws.path(), ws.secret()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	env := baseEnv()
	set := func(k, v string) { env = append(env, k+"="+v) }
	set("DISPATCH_JOB_ID", cfg.JobID.String())
	set("DISPATCH_WORKDIR", ws.dir)
	set("DISPATCH_LIMIT", spec.Limit)
	if spec.Project != nil {
		ws.projectDir = filepath.Join(ws.dir, "project")
		set("DISPATCH_PROJECT_DIR", ws.projectDir)
	}

	if spec.Inventory != nil {
		ws.inventory = filepath.Join(ws.dir, "inventory.json")
		if err := writeJSON(ws.inventory, spec.Inventory, 0o600); err != nil {
			return nil, err
		}
		set("DISPATCH_INVENTORY", ws.inventory)
	}
	ws.vars = filepath.Join(ws.dir, "vars.json")
	if err := writeJSON(ws.vars, spec.ExtraVars, 0o600); err != nil {
		return nil, err
	}
	set("DISPATCH_VARS", ws.vars)

	type credFile struct {
		Name   string            `json:"name"`
		Kind   string            `json:"kind"`
		Inputs map[string]string `json:"inputs"`
	}
	all := make([]credFile, 0, len(spec.Credentials))
	credDir := filepath.Join(ws.secrets, "credentials")
	used := map[string]bool{}
	for _, c := range spec.Credentials {
		all = append(all, credFile{Name: c.Name, Kind: string(c.Kind), Inputs: c.Inputs})
		dir := filepath.Join(credDir, uniqueSlug(c.Name, used))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		for name, value := range c.Inputs {
			// ssh rejects private keys without a trailing newline.
			if name == "private_key" && !strings.HasSuffix(value, "\n") {
				value += "\n"
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
				return nil, err
			}
		}
	}
	credsJSON := filepath.Join(ws.secrets, "credentials.json")
	if err := writeJSON(credsJSON, all, 0o600); err != nil {
		return nil, err
	}
	set("DISPATCH_CREDENTIALS", credsJSON)
	set("DISPATCH_CREDENTIALS_DIR", credDir)
	ws.env = env
	return ws, nil
}

// baseEnv is the runner's environment without its own settings, which the
// command has no use for.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DISPATCH_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), mode)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9_.-]+`)

// uniqueSlug turns a credential name into a directory name.
func uniqueSlug(name string, used map[string]bool) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-.")
	if slug == "" {
		slug = "credential"
	}
	candidate := slug
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", slug, i)
	}
	used[candidate] = true
	return candidate
}
