package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
	"dispatch-ops/internal/testdb"
)

// Info finding: playbook_stats events, which are still kept past the
// output cap so the recap survives, can't take a job more than 1 MiB past
// it, however many the runner sends.
func TestRecapAllowanceIsBounded(t *testing.T) {
	pool, _ := testdb.Open(t, "store")
	st := store.New(pool)
	ctx := context.Background()
	job, err := st.CreateJob(ctx, store.NewJob{Name: "recap", LaunchType: "api", Config: store.JobConfig{
		JobType: "ansible", TimeoutSeconds: 60,
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: uuid.New(), Image: "local/test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'running', started_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	const limit = 1000
	seq := int64(0)
	appendEvents := func(evs ...store.JobEvent) {
		t.Helper()
		if _, err := st.AppendJobEvents(ctx, job.ID, evs, limit); err != nil {
			t.Fatal(err)
		}
	}
	next := func(typ, stdout string, data map[string]any) store.JobEvent {
		seq++
		return store.JobEvent{Seq: seq, Time: time.Now(), Type: typ, Stdout: stdout, Data: data}
	}
	appendEvents(next("stdout", strings.Repeat("x", 600), nil), next("stdout", strings.Repeat("x", 600), nil))

	// A real recap past the cap is kept.
	recap := map[string]any{"hosts": map[string]any{"web1": map[string]any{"ok": 3, "failures": 0}}}
	appendEvents(next("playbook_stats", "", recap))
	// Forged ones (64 KiB each) are kept only up to the allowance.
	big := map[string]any{"hosts": map[string]any{"h": strings.Repeat("y", 64<<10)}}
	for range 40 {
		appendEvents(next("playbook_stats", "", big))
	}

	var used int64
	var truncated bool
	var stats int
	if err := pool.QueryRow(ctx, `SELECT output_bytes, output_truncated FROM jobs WHERE id = $1`, job.ID).Scan(&used, &truncated); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_events WHERE job_id = $1 AND event_type = 'playbook_stats'`, job.ID).Scan(&stats); err != nil {
		t.Fatal(err)
	}
	if !truncated || used > limit+1<<20 {
		t.Errorf("output_bytes = %d (truncated %v), want at most %d", used, truncated, limit+1<<20)
	}
	// The real recap, and as many forged ones as fit in 1 MiB.
	if stats < 2 || stats > 1+16 {
		t.Errorf("%d playbook_stats events kept", stats)
	}
}
