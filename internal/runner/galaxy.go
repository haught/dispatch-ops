package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dispatch-ops/internal/runner/runnerapi"
)

// Ansible's default search paths, which the job's own install directories
// go in front of.
const (
	defaultCollectionsPath = "~/.ansible/collections:/usr/share/ansible/collections"
	defaultRolesPath       = "~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles"
)

// galaxyStep is one ansible-galaxy install from a requirements file.
type galaxyStep struct {
	file string // relative to the project
	kind string // "collection", "role", or "" for a file with both
}

// requirementsFiles returns the requirements files in the project, in the
// order they are installed: roles/requirements.yml, then
// collections/requirements.yml, then requirements.yml at the top, which
// may list both roles and collections. Each may also end in .yaml.
func requirementsFiles(projectDir string) []galaxyStep {
	var steps []galaxyStep
	for _, s := range []galaxyStep{
		{file: "roles/requirements", kind: "role"},
		{file: "collections/requirements", kind: "collection"},
		{file: "requirements", kind: ""},
	} {
		for _, ext := range []string{".yml", ".yaml"} {
			fi, err := os.Stat(filepath.Join(projectDir, s.file+ext))
			if err == nil && fi.Mode().IsRegular() {
				steps = append(steps, galaxyStep{file: s.file + ext, kind: s.kind})
				break
			}
		}
	}
	return steps
}

// installRequirements installs the roles and collections the project's
// requirements files list, into directories in the workspace, and records
// in ws.ansibleEnv the search paths that make ansible-playbook find them.
// ansible-galaxy uses the project's SCM credential, so requirements can
// come from the same private Git server as the project; git gets it only
// for that server (see Askpass). Status messages go
// to say and ansible-galaxy's output to out.
func installRequirements(ctx context.Context, cfg Config, ws *workspace, p *runnerapi.RunnerProject, verbosity int, say func(string), out io.Writer) error {
	steps := requirementsFiles(ws.projectDir)
	if len(steps) == 0 {
		return nil
	}
	collections, roles := ws.path("ansible", "collections"), ws.path("ansible", "roles")
	for _, d := range []string{collections, roles} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	paths := []string{
		"ANSIBLE_COLLECTIONS_PATH=" + collections + ":" + defaultCollectionsPath,
		"ANSIBLE_ROLES_PATH=" + roles + ":" + defaultRolesPath,
	}

	env, cleanup, err := gitEnv(cfg, ws, p)
	defer cleanup()
	if err != nil {
		return err
	}
	tmp := ws.path("ansible", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	env = append(env, paths...)
	env = append(env,
		"ANSIBLE_LOCAL_TEMP="+tmp,
		"ANSIBLE_GALAXY_CACHE_DIR="+ws.path("ansible", "galaxy_cache"),
		"ANSIBLE_NOCOLOR=True",
		"ANSIBLE_FORCE_COLOR=False",
		"PYTHONUNBUFFERED=1",
	)

	for _, s := range steps {
		argv := []string{"ansible-galaxy"}
		switch s.kind {
		case "collection":
			argv = append(argv, "collection", "install", "-r", s.file, "-p", collections)
		case "role":
			argv = append(argv, "role", "install", "-r", s.file, "-p", roles)
		default:
			// Without -p, roles and collections both install, into the
			// first of the search paths set above.
			argv = append(argv, "install", "-r", s.file)
		}
		if verbosity > 0 {
			argv = append(argv, "-"+strings.Repeat("v", min(verbosity, 5)))
		}
		say(fmt.Sprintf("Installing requirements from %s", s.file))
		if err := runGalaxy(ctx, ws.projectDir, env, argv, out); err != nil {
			return fmt.Errorf("%s: %w", s.file, err)
		}
	}
	ws.ansibleEnv = paths
	return nil
}

// runGalaxy runs an ansible-galaxy command in dir with its output to out.
func runGalaxy(ctx context.Context, dir string, env, argv []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	// Stopping ansible-galaxy stops the git or ssh it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("ansible-galaxy is not installed in the execution environment image")
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return fmt.Errorf("ansible-galaxy exited with code %d", exitErr.ExitCode())
	default:
		return err
	}
}
