package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/runner"
	"dispatch-ops/internal/store"
)

// jobEnv is a signed-in client plus the store, for tests that act as the
// task manager.
type jobEnv struct {
	t  *testing.T
	c  *client
	st *store.Store
	ee string
	// tokens holds the runner token of every job claimed so far: a claim
	// takes all pending jobs, not just the one asked for.
	tokens map[uuid.UUID]string
}

func newJobEnv(t *testing.T) *jobEnv {
	t.Helper()
	srv, st, username, password := startServer(t)
	c := login(t, srv.URL, username, password)
	ee := c.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	return &jobEnv{t: t, c: c, st: st, ee: ee["id"].(string), tokens: map[uuid.UUID]string{}}
}

// launch creates a container template and launches it.
func (e *jobEnv) launch(tmpl map[string]any) string {
	e.t.Helper()
	tmpl["name"] = unique("jt")
	tmpl["jobType"] = "container"
	tmpl["executionEnvironmentId"] = e.ee
	jt := e.c.mustDo("POST", "/api/v1/job-templates", tmpl, 201)
	job := e.c.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch", nil, 201)
	return job["id"].(string)
}

// claim does the task manager's part: it moves the job to waiting and
// returns its runner token.
func (e *jobEnv) claim(jobID string) string {
	e.t.Helper()
	id := uuid.MustParse(jobID)
	if token, ok := e.tokens[id]; ok {
		return token
	}
	_, err := e.st.ClaimJobs(context.Background(), 1<<30, func(j store.Job) (store.Dispatch, error) {
		secret, hash, _, err := auth.NewToken(auth.JobTokenPrefix)
		e.tokens[j.ID] = secret
		return store.Dispatch{TokenHash: hash, K8sNamespace: "test", K8sJobName: "test-" + j.ID.String()}, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	token, ok := e.tokens[id]
	if !ok {
		e.t.Fatalf("job %s was not claimed", jobID)
	}
	return token
}

func (e *jobEnv) runnerConfig(jobID, token string, out *bytes.Buffer) runner.Config {
	return runner.Config{
		APIURL: e.c.base, JobID: uuid.MustParse(jobID), Token: token, WorkDir: e.t.TempDir(),
		Version: "test", Stdout: out, Heartbeat: 100 * time.Millisecond, KillGrace: 2 * time.Second,
	}
}

// stdout returns the job's output events joined by newlines.
func (e *jobEnv) stdout(jobID string) string {
	e.t.Helper()
	var lines []string
	for _, it := range e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?limit=5000", nil, 200)["items"].([]any) {
		ev := it.(map[string]any)
		if ev["type"] == "stdout" {
			lines = append(lines, ev["stdout"].(string))
		}
	}
	return strings.Join(lines, "\n")
}

// sseEvent is one server-sent event.
type sseEvent struct {
	name, id string
	data     map[string]any
}

// follow reads the job's live stream until it ends, in the background.
func (e *jobEnv) follow(jobID string) <-chan []sseEvent {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.c.base+"/api/v1/jobs/"+jobID+"/stream", nil)
	req.AddCookie(e.c.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan []sseEvent, 1)
	go func() {
		defer resp.Body.Close()
		var events []sseEvent
		var cur sseEvent
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data)
			case line == "" && cur.name != "":
				events = append(events, cur)
				if cur.name == "end" {
					out <- events
					return
				}
				cur = sseEvent{}
			}
		}
		out <- events
	}()
	return out
}

func TestJobRunsAndStreamsOutput(t *testing.T) {
	e := newJobEnv(t)
	inv := e.c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)["id"].(string)
	e.c.mustDo("POST", "/api/v1/inventories/"+inv+"/hosts", map[string]any{"name": "web1"}, 201)
	// Credential files go in a directory named after the credential.
	cred := e.c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("API Key"), "kind": "generic", "inputs": map[string]string{"TOKEN": "s3cret"},
	}, 201)

	script := `echo "hello from $DISPATCH_JOB_ID"
grep -q '"web1"' "$DISPATCH_INVENTORY" && echo web1
cat "$DISPATCH_VARS" | tr -d ' \n'; echo
echo "token=$(cat "$DISPATCH_CREDENTIALS_DIR"/api-key-*/TOKEN)"
echo "limit=$DISPATCH_LIMIT"
echo "api=${DISPATCH_API_URL:-unset}"
echo oops >&2
exit 3`
	jobID := e.launch(map[string]any{
		"command": []string{"sh", "-c", script}, "inventoryId": inv, "credentialIds": []string{cred["id"].(string)},
		"extraVars": map[string]any{"color": "blue"}, "limit": "web*",
	})
	stream := e.follow(jobID)
	token := e.claim(jobID)

	var out bytes.Buffer
	if err := runner.Run(context.Background(), e.runnerConfig(jobID, token, &out)); err != nil {
		t.Fatalf("runner: %v\n%s", err, out.String())
	}

	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "failed" || job["exitCode"].(float64) != 3 || job["startedAt"] == nil || job["finishedAt"] == nil {
		t.Errorf("job = %s", jsonOf(job))
	}
	if !strings.Contains(job["failureReason"].(string), "exited with code 3") {
		t.Errorf("failureReason = %q", job["failureReason"])
	}

	want := strings.Join([]string{
		"hello from " + jobID, "web1", `{"color":"blue"}`, "token=s3cret", "limit=web*", "api=unset", "oops",
	}, "\n")
	if got := e.stdout(jobID); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}

	// Events page by sequence number.
	page := e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?afterSeq=2&limit=2", nil, 200)["items"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["seq"].(float64) != 3 {
		t.Errorf("events page = %s", jsonOf(page))
	}

	// The live stream saw the status changes, every event in order, and the end.
	select {
	case events := <-stream:
		var statuses []string
		var lastSeq float64
		var outputs int
		for _, ev := range events {
			switch ev.name {
			case "status":
				statuses = append(statuses, ev.data["status"].(string))
			case "job_event":
				seq := ev.data["seq"].(float64)
				if seq != lastSeq+1 {
					t.Errorf("stream skipped from seq %v to %v", lastSeq, seq)
				}
				lastSeq = seq
				if ev.data["type"] == "stdout" {
					outputs++
				}
			}
		}
		if statuses[0] != "pending" || statuses[len(statuses)-1] != "failed" {
			t.Errorf("stream statuses = %v", statuses)
		}
		if outputs != 7 || events[len(events)-1].name != "end" {
			t.Errorf("stream had %d output events, last event %q", outputs, events[len(events)-1].name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}

	// A stream of a finished job replays from Last-Event-ID and ends.
	req, _ := http.NewRequest("GET", e.c.base+"/api/v1/jobs/"+jobID+"/stream", nil)
	req.AddCookie(e.c.cookie)
	req.Header.Set("Last-Event-ID", "5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if s := buf.String(); strings.Contains(s, "id: 5\n") || !strings.Contains(s, "id: 6\n") || !strings.Contains(s, "event: end") {
		t.Errorf("resumed stream:\n%s", s)
	}

	// The job token stopped working when the job finished.
	r := &client{t: t, base: e.c.base, bearer: token}
	resp, body := r.do("POST", "/runner/v1/jobs/"+jobID+"/heartbeat", nil)
	expectStatus(t, resp, body, 401)
}

func TestRunnerTokenIsScopedToItsJob(t *testing.T) {
	e := newJobEnv(t)
	a := e.launch(map[string]any{"command": []string{"true"}})
	b := e.launch(map[string]any{"command": []string{"true"}})
	tokenA := e.claim(a)
	e.claim(b)

	r := &client{t: t, base: e.c.base, bearer: tokenA}
	resp, body := r.do("GET", "/runner/v1/jobs/"+b+"/spec?protocol=1&version=x", nil)
	expectStatus(t, resp, body, 403)
	// Job tokens are not user credentials, and user credentials don't work
	// on the runner API.
	resp, body = r.do("GET", "/api/v1/jobs", nil)
	expectStatus(t, resp, body, 401)
	resp, body = e.c.do("GET", "/runner/v1/jobs/"+a+"/spec?protocol=1&version=x", nil)
	expectStatus(t, resp, body, 401)

	// The spec can be fetched once.
	resp, body = r.do("GET", "/runner/v1/jobs/"+a+"/spec?protocol=1&version=x", nil)
	expectStatus(t, resp, body, 200)
	resp, body = r.do("GET", "/runner/v1/jobs/"+a+"/spec?protocol=1&version=x", nil)
	expectStatus(t, resp, body, 409)

	// Retried event batches are stored once.
	batch := map[string]any{"events": []map[string]any{{"seq": 1, "type": "stdout", "stdout": "a\x00b"}}}
	r.mustDo("POST", "/runner/v1/jobs/"+a+"/events", batch, 200)
	r.mustDo("POST", "/runner/v1/jobs/"+a+"/events", batch, 200)
	if got := e.stdout(a); got != "ab" {
		t.Errorf("stdout = %q", got)
	}
	r.mustDo("POST", "/runner/v1/jobs/"+a+"/finish", map[string]any{"status": "successful", "exitCode": 0}, 204)
}

func TestUnsupportedRunnerProtocolFailsTheJob(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"true"}})
	r := &client{t: t, base: e.c.base, bearer: e.claim(jobID)}
	resp, body := r.do("GET", "/runner/v1/jobs/"+jobID+"/spec?protocol=99&version=9.9.9", nil)
	expectStatus(t, resp, body, 400)

	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "error" || !strings.Contains(job["failureReason"].(string), "protocol 99") {
		t.Errorf("job = %s", jsonOf(job))
	}
}

func TestCancelRunningJob(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"sh", "-c", "trap 'echo got TERM; exit 143' TERM; echo started; sleep 30 & wait"}})
	token := e.claim(jobID)

	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		cfg := e.runnerConfig(jobID, token, nil)
		cfg.Stdout = &out
		done <- runner.Run(context.Background(), cfg)
	}()
	waitFor(t, func() bool { return strings.Contains(e.stdout(jobID), "started") })

	job := e.c.mustDo("POST", "/api/v1/jobs/"+jobID+"/cancel", nil, 202)
	if job["status"] != "running" || job["cancelRequested"] != true {
		t.Errorf("cancel response = %s", jsonOf(job))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("runner did not stop after cancel\n%s", out.String())
	}
	job = e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "canceled" {
		t.Errorf("job = %s", jsonOf(job))
	}
	if !strings.Contains(e.stdout(jobID), "got TERM") {
		t.Errorf("the command did not get SIGTERM; stdout:\n%s", e.stdout(jobID))
	}
}

func TestJobTimeout(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"sleep", "30"}, "timeoutSeconds": 1})
	var out bytes.Buffer
	start := time.Now()
	if err := runner.Run(context.Background(), e.runnerConfig(jobID, e.claim(jobID), &out)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "failed" || !strings.Contains(job["failureReason"].(string), "Timed out") {
		t.Errorf("job = %s", jsonOf(job))
	}
}

func TestCommandNotFound(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"no-such-command-xyz"}})
	var out bytes.Buffer
	if err := runner.Run(context.Background(), e.runnerConfig(jobID, e.claim(jobID), &out)); err != nil {
		t.Fatal(err)
	}
	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "error" || !strings.Contains(job["failureReason"].(string), "no-such-command-xyz") {
		t.Errorf("job = %s", jsonOf(job))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
