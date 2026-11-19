package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A job's log is its output lines, runner messages and structured events
// with output; structured events without output are left out of the log,
// the live stream and the download.
func TestJobLog(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"true"}})
	r := &client{t: t, base: e.c.base, bearer: e.claim(jobID)}
	r.mustDo("GET", "/runner/v1/jobs/"+jobID+"/spec?protocol=4&version=x", nil, 200)

	ev := func(seq int, typ, host, stdout string) map[string]any {
		m := map[string]any{"seq": seq, "type": typ, "stdout": stdout}
		if host != "" {
			m["hostName"] = host
			m["taskSeq"] = 2
			m["data"] = map[string]any{"changed": false}
		}
		if typ == "task_start" {
			m["data"] = map[string]any{"task": "Greet", "action": "debug", "play": "P"}
		}
		return m
	}
	r.mustDo("POST", "/runner/v1/jobs/"+jobID+"/events", map[string]any{"events": []any{
		ev(1, "stdout", "", "\x1b[0;32mfirst\x1b[0m"),
		ev(2, "task_start", "", "\nTASK [Greet] ***"),
		ev(3, "host_ok", "web1", "ok: [web1]"),
		ev(4, "host_ok", "web2", ""), // display_ok_hosts off: no output
		ev(5, "runner", "", "a runner message"),
		ev(6, "stdout", "", ""), // an empty line is output
	}}, 200)

	seqs := func(path string) string {
		var out []string
		for _, it := range e.c.mustDo("GET", path, nil, 200)["items"].([]any) {
			out = append(out, jsonOf(it.(map[string]any)["seq"]))
		}
		return strings.Join(out, ",")
	}
	base := "/api/v1/jobs/" + jobID + "/events"
	if got := seqs(base + "?log=true"); got != "1,2,3,5,6" {
		t.Errorf("log = %s", got)
	}
	if got := seqs(base + "?log=true&beforeSeq=9007199254740991&limit=2"); got != "5,6" {
		t.Errorf("log tail = %s", got)
	}
	if got := seqs(base + "?log=true&beforeSeq=5&limit=2"); got != "2,3" {
		t.Errorf("log before 5 = %s", got)
	}
	// A host's results name their task.
	host := e.c.mustDo("GET", base+"?hostName=web1", nil, 200)["items"].([]any)
	if task := host[0].(map[string]any)["task"].(map[string]any); task["name"] != "Greet" || task["play"] != "P" {
		t.Errorf("web1's task = %v", task)
	}

	stream := e.follow(jobID)
	r.mustDo("POST", "/runner/v1/jobs/"+jobID+"/finish", map[string]any{"status": "successful", "exitCode": 0}, 204)
	select {
	case events := <-stream:
		var logSeqs []string
		var hosts int
		for _, ev := range events {
			switch ev.name {
			case "job_event":
				logSeqs = append(logSeqs, ev.id)
			case "hosts":
				hosts++
			}
		}
		if strings.Join(logSeqs, ",") != "1,2,3,5,6" || hosts != 1 {
			t.Errorf("stream: log events %v, %d hosts events", logSeqs, hosts)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}

	download := func(query string) (string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.c.base+"/api/v1/jobs/"+jobID+"/output"+query, nil)
		req.AddCookie(e.c.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("download: %d %s", resp.StatusCode, body)
		}
		return string(body), resp.Header
	}
	text, hdr := download("")
	if want := "\x1b[0;32mfirst\x1b[0m\n\nTASK [Greet] ***\nok: [web1]\na runner message\n\n"; text != want {
		t.Errorf("log = %q, want %q", text, want)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || !strings.Contains(hdr.Get("Content-Disposition"), "attachment") {
		t.Errorf("headers = %v", hdr)
	}
	if text, _ := download("?ansi=false"); !strings.HasPrefix(text, "first\n") {
		t.Errorf("log without ANSI = %q", text)
	}

	// Users without access to the job can't read it.
	name := unique("user")
	e.c.mustDo("POST", "/api/v1/users", map[string]any{"username": name, "password": "another-long-password"}, 201)
	other := login(t, e.c.base, name, "another-long-password")
	if resp, body := other.do("GET", "/api/v1/jobs/"+jobID+"/output", nil); resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Errorf("download by another user: %d %v", resp.StatusCode, body)
	}
}
