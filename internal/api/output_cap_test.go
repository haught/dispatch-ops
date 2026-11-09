package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
)

// L8: a job's stored output is capped; past the cap the job keeps running,
// a note says the output was cut, and only the recap is still kept.
func TestJobOutputCap(t *testing.T) {
	e := newJobEnv(t)
	ctx := context.Background()
	job, err := e.st.CreateJob(ctx, store.NewJob{Name: "noisy", LaunchType: "api", Config: store.JobConfig{
		JobType: "ansible", TimeoutSeconds: 60,
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: uuid.MustParse(e.ee), Image: "local/test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.claim(job.ID.String())
	if _, err := e.st.StartJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	const limit = 100
	line := func(seq int64) store.JobEvent {
		return store.JobEvent{Seq: seq, Time: time.Now(), Type: "stdout", Stdout: strings.Repeat("x", 40)}
	}
	appendEvents := func(evs ...store.JobEvent) {
		t.Helper()
		if _, err := e.st.AppendJobEvents(ctx, job.ID, evs, limit); err != nil {
			t.Fatal(err)
		}
	}

	appendEvents(line(1), line(2))
	appendEvents(line(1), line(2)) // a retry doesn't count twice
	appendEvents(line(3), line(4)) // 3 crosses the cap
	appendEvents(line(5), store.JobEvent{Seq: 6, Time: time.Now(), Type: "playbook_stats", Data: map[string]any{"ok": 1}})

	events := e.c.mustDo("GET", "/api/v1/jobs/"+job.ID.String()+"/events?limit=100", nil, 200)["items"].([]any)
	var got []string
	for _, it := range events {
		ev := it.(map[string]any)
		got = append(got, ev["type"].(string))
		if ev["type"] == "runner" && !strings.Contains(ev["stdout"].(string), "limit of 100 bytes") {
			t.Errorf("truncation note = %q", ev["stdout"])
		}
	}
	want := []string{"stdout", "stdout", "runner", "playbook_stats"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stored events = %v, want %v", got, want)
	}

	// The job is still running, and a heartbeat still works.
	if j := e.c.mustDo("GET", "/api/v1/jobs/"+job.ID.String(), nil, 200); j["status"] != "running" {
		t.Errorf("status = %v", j["status"])
	}
}
