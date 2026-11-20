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

type eventsJob struct {
	t    *testing.T
	ctx  context.Context
	st   *store.Store
	id   uuid.UUID
	seq  int64
	stat func() (used int64, truncated bool, events int)
}

func newEventsJob(t *testing.T) *eventsJob {
	t.Helper()
	pool, _ := testdb.Open(t, "store")
	st := store.New(pool)
	ctx := context.Background()
	job, err := st.CreateJob(ctx, store.NewJob{Name: "events", LaunchType: "api", Config: store.JobConfig{
		JobType: "ansible", TimeoutSeconds: 60,
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: uuid.New(), Image: "local/test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'running', started_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	j := &eventsJob{t: t, ctx: ctx, st: st, id: job.ID}
	j.stat = func() (used int64, truncated bool, events int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT output_bytes, output_truncated, (SELECT count(*) FROM job_events WHERE job_id = $1)
			FROM jobs WHERE id = $1`, job.ID).Scan(&used, &truncated, &events); err != nil {
			t.Fatal(err)
		}
		return
	}
	return j
}

func (j *eventsJob) next(typ, host, stdout string, data map[string]any) store.JobEvent {
	j.seq++
	e := store.JobEvent{Seq: j.seq, Time: time.Now(), Type: typ, Stdout: stdout, Data: data}
	if host != "" {
		e.HostName = &host
	}
	return e
}

func (j *eventsJob) send(maxOutput int64, evs ...store.JobEvent) {
	j.t.Helper()
	if _, err := j.st.AppendJobEvents(j.ctx, j.id, evs, maxOutput); err != nil {
		j.t.Fatal(err)
	}
}

func (j *eventsJob) hosts() map[string]store.JobHostSummary {
	j.t.Helper()
	hs, err := j.st.ListJobHostSummaries(j.ctx, j.id, store.JobHostFilter{}, store.Page{})
	if err != nil {
		j.t.Fatal(err)
	}
	out := map[string]store.JobHostSummary{}
	for _, h := range hs.Items {
		out[h.HostName] = h
	}
	return out
}

// Host results are counted from every event, including those past the
// output cap, which aren't stored.
func TestHostResultsSurviveOutputCap(t *testing.T) {
	j := newEventsJob(t)
	const limit = 1000
	j.send(limit, j.next("stdout", "", strings.Repeat("x", 900), nil))
	j.send(limit,
		j.next("host_ok", "web1", "", map[string]any{"changed": true}),
		j.next("host_failed", "web2", "", map[string]any{"msg": strings.Repeat("boom", 100)}),
		j.next("host_failed", "web1", "", map[string]any{"ignored": true}),
		j.next("host_unreachable", "db1", "", nil),
		j.next("host_skipped", "db2", "", nil),
	)
	used, truncated, events := j.stat()
	if !truncated || used > limit {
		t.Errorf("output_bytes = %d (truncated %v), want at most %d", used, truncated, limit)
	}
	want := map[string]store.JobHostSummary{
		"web1": {HostName: "web1", OK: 2, Changed: 1, Ignored: 1},
		"web2": {HostName: "web2", Failed: 1},
		"db1":  {HostName: "db1", Unreachable: 1},
		"db2":  {HostName: "db2", Skipped: 1},
	}
	got := j.hosts()
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
	if s := got["web1"].Status(); s != store.HostChanged {
		t.Errorf("web1 status = %s", s)
	}

	// The recap replaces the counts, and isn't stored as an event.
	j.send(limit, j.next("playbook_stats", "", "", map[string]any{"hosts": map[string]any{
		"web1": map[string]any{"ok": 5, "changed": 2, "rescued": 1, "ignored": 1},
		"web2": map[string]any{"ok": 1, "failures": 1},
	}}))
	got = j.hosts()
	if w := (store.JobHostSummary{HostName: "web1", OK: 5, Changed: 2, Rescued: 1, Ignored: 1}); got["web1"] != w {
		t.Errorf("web1 after recap = %+v, want %+v", got["web1"], w)
	}
	if _, _, after := j.stat(); after != events {
		t.Errorf("%d events stored after the recap, want %d", after, events)
	}

	hs, err := j.st.ListJobHostSummaries(j.ctx, j.id, store.JobHostFilter{Statuses: []string{store.HostFailed, store.HostUnreachable}}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if hs.Total != 2 || len(hs.Items) != 2 || hs.Items[0].HostName != "db1" || hs.Items[1].HostName != "web2" {
		t.Errorf("failed hosts = %+v", hs)
	}
	if hs.ByStatus[store.HostChanged] != 1 || hs.ByStatus[store.HostSkipped] != 1 {
		t.Errorf("counts = %v", hs.ByStatus)
	}
}

// A retried batch (whose first try was stored, but whose response was
// lost) changes nothing.
func TestRetriedBatchIsIgnored(t *testing.T) {
	j := newEventsJob(t)
	batch := []store.JobEvent{j.next("stdout", "", "hello", nil), j.next("host_ok", "web1", "", nil)}
	j.send(0, batch...)
	used, _, events := j.stat()
	j.send(0, batch...)
	j.send(0, batch[1:]...)
	if u, _, e := j.stat(); u != used || e != events {
		t.Errorf("after retries: %d bytes, %d events; want %d, %d", u, e, used, events)
	}
	if h := j.hosts()["web1"]; h.OK != 1 {
		t.Errorf("web1 ok = %d, want 1", h.OK)
	}
}

// A job can't record results for more than maxJobHosts hosts, however many
// it reports; hosts it has keep counting.
func TestJobHostsAreCapped(t *testing.T) {
	defer store.SetMaxJobHosts(3)()
	j := newEventsJob(t)
	j.send(0, j.next("host_ok", "a", "", nil), j.next("host_ok", "b", "", nil))
	recap := map[string]any{}
	for _, h := range []string{"a", "c", "d", "e"} {
		recap[h] = map[string]any{"ok": 1}
	}
	j.send(0, j.next("playbook_stats", "", "", map[string]any{"hosts": recap}))
	j.send(0, j.next("host_ok", "f", "", nil), j.next("host_ok", "b", "", nil))
	got := j.hosts()
	if len(got) != 3 || got["b"].OK != 2 {
		t.Errorf("hosts = %v, want 3, with b counted twice", got)
	}
}
