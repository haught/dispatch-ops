package taskmanager

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/store"
)

func (e *env) template(allowOverrides ...string) store.JobTemplate {
	e.t.Helper()
	t, err := e.st.CreateJobTemplate(e.ctx, store.JobTemplateFields{
		Name: "jt-" + uuid.NewString()[:8], JobType: "container", ExecutionEnvironmentID: e.ee.ID,
		Command: []string{"true"}, TimeoutSeconds: 60, AllowOverrides: allowOverrides,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

func TestSchedulesLaunchJobs(t *testing.T) {
	e := newEnv(t)
	tmpl := e.template("limit")
	// Schedules run with their owner's access; this one may launch anything.
	owner, err := e.st.CreateLocalUser(e.ctx, store.NewLocalUser{Username: "owner-" + uuid.NewString()[:8], PasswordHash: "x", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	limit := "web*"
	good, err := e.st.CreateSchedule(e.ctx, store.ScheduleTarget{TemplateID: &tmpl.ID}, &owner.ID, store.ScheduleFields{
		Name: "nightly", Cron: "0 3 * * *", Timezone: "Europe/Berlin", Enabled: true, NextRunAt: &past,
		Overrides: store.LaunchOverrides{Limit: &limit},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The template doesn't allow overriding extra vars.
	bad, err := e.st.CreateSchedule(e.ctx, store.ScheduleTarget{TemplateID: &tmpl.ID}, &owner.ID, store.ScheduleFields{
		Name: "bad", Cron: "@hourly", Timezone: "UTC", Enabled: true, NextRunAt: &past,
		Overrides: store.LaunchOverrides{ExtraVars: map[string]any{"x": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if _, err := e.st.CreateSchedule(e.ctx, store.ScheduleTarget{TemplateID: &tmpl.ID}, &owner.ID, store.ScheduleFields{
		Name: "later", Cron: "@hourly", Timezone: "UTC", Enabled: true, NextRunAt: &future,
	}); err != nil {
		t.Fatal(err)
	}

	e.m.Tick(e.ctx)

	g, _ := e.st.GetSchedule(e.ctx, good.ID)
	if g.LastJobID == nil || g.LastError != "" || g.LastRunAt == nil {
		t.Fatalf("schedule after run = %+v", g)
	}
	if g.NextRunAt == nil || !g.NextRunAt.After(time.Now()) {
		t.Errorf("next run = %v, want in the future", g.NextRunAt)
	}
	// 03:00 in Berlin is 01:00 or 02:00 UTC.
	if h := g.NextRunAt.UTC().Hour(); h != 1 && h != 2 {
		t.Errorf("next run %v is not 03:00 Europe/Berlin", g.NextRunAt)
	}
	j := e.job(*g.LastJobID)
	if j.LaunchType != "schedule" || j.Schedule == nil || j.Schedule.ID != good.ID || j.Config.Limit != "web*" || j.LaunchedBy != nil {
		t.Errorf("scheduled job = %+v", j)
	}
	if j.Status != store.JobWaiting {
		t.Errorf("scheduled job was not dispatched in the same pass: %s", j.Status)
	}

	b, _ := e.st.GetSchedule(e.ctx, bad.ID)
	if b.LastJobID != nil || !strings.Contains(b.LastError, `overriding "extraVars"`) || b.NextRunAt == nil || !b.NextRunAt.After(time.Now()) {
		t.Errorf("bad schedule = %+v", b)
	}

	// Each run launches once.
	e.m.Tick(e.ctx)
	jobs, total, err := e.st.ListJobs(e.ctx, store.JobFilter{TemplateID: &tmpl.ID}, store.Page{Limit: 10})
	if err != nil || total != 1 {
		t.Errorf("template has %d jobs (%v), want 1", total, jobs)
	}
}

// webhookServer records the requests it receives and answers with status.
type webhookServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	got    []*http.Request
	bodies [][]byte
}

func newWebhookServer(t *testing.T) *webhookServer {
	w := &webhookServer{status: 200}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.got = append(w.got, r)
		w.bodies = append(w.bodies, body)
		rw.WriteHeader(w.status)
	}))
	t.Cleanup(w.Close)
	return w
}

func (e *env) webhookTarget(url string, secrets map[string]string) store.NotificationTarget {
	e.t.Helper()
	n := store.NotificationTarget{ID: uuid.New(), Name: "hook-" + uuid.NewString()[:8], Kind: notify.KindWebhook,
		Config: map[string]any{"url": url}}
	if err := e.m.NotifyVault.Seal(&n, secrets); err != nil {
		e.t.Fatal(err)
	}
	n, err := e.st.CreateNotificationTarget(e.ctx, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestNotificationsAreDelivered(t *testing.T) {
	e := newEnv(t)
	e.m.NotifyVault = notify.Vault{Keys: e.m.Vault.Keys}
	e.m.Notifier = &notify.Sender{ExternalURL: "https://dispatch.example.com", Version: "test"}
	hook := newWebhookServer(t)
	target := e.webhookTarget(hook.URL, map[string]string{"signingSecret": "s3cret", "authorization": "Bearer abc"})
	tmpl := e.template()
	if _, err := e.st.SetTemplateNotifications(e.ctx, tmpl.ID, []store.TemplateNotification{
		{TargetID: target.ID, Events: []string{"started", "failed"}},
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := e.st.ResolveJobConfig(e.ctx, tmpl, store.LaunchOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.st.CreateJob(e.ctx, store.NewJob{TemplateID: &tmpl.ID, Name: tmpl.Name, LaunchType: "api", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.m.Tick(e.ctx)
	if _, err := e.st.StartJob(e.ctx, j.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishJob(e.ctx, j.ID, store.JobFailed, new(2), "exit status 2", ""); err != nil {
		t.Fatal(err)
	}

	if n, err := e.m.SendNotifications(e.ctx); err != nil || n != 2 {
		t.Fatalf("SendNotifications = %d, %v", n, err)
	}
	ds, _ := e.st.ListJobDeliveries(e.ctx, j.ID)
	if len(ds) != 2 || ds[0].Event != "started" || ds[1].Event != "failed" || ds[0].Status != store.DeliverySent || ds[1].Status != store.DeliverySent {
		t.Fatalf("deliveries = %+v", ds)
	}
	if len(hook.got) != 2 {
		t.Fatalf("webhook got %d requests", len(hook.got))
	}
	// Deliveries in a batch are sent concurrently.
	i := 0
	if hook.got[0].Header.Get("X-Dispatch-Event") == "failed" {
		i = 1
	}
	if hook.got[i].Header.Get("X-Dispatch-Event") != "started" {
		t.Errorf("events = %s, %s", hook.got[0].Header.Get("X-Dispatch-Event"), hook.got[1].Header.Get("X-Dispatch-Event"))
	}
	r, body := hook.got[1-i], hook.bodies[1-i]
	if r.Header.Get("Authorization") != "Bearer abc" || r.Header.Get("X-Dispatch-Event") != "failed" {
		t.Errorf("headers = %v", r.Header)
	}
	if want := "sha256=" + notify.Sign("s3cret", r.Header.Get("X-Dispatch-Timestamp"), body); r.Header.Get("X-Dispatch-Signature") != want {
		t.Errorf("signature = %q, want %q", r.Header.Get("X-Dispatch-Signature"), want)
	}
	var p notify.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "failed" || p.Job.ID != j.ID || *p.Job.ExitCode != 2 || p.Job.URL != "https://dispatch.example.com/jobs/"+j.ID.String() {
		t.Errorf("payload = %+v", p.Job)
	}

	// Server errors are retried later; client errors are not.
	hook.status = 503
	if _, err := e.st.SetTemplateNotifications(e.ctx, tmpl.ID, []store.TemplateNotification{
		{TargetID: target.ID, Events: []string{"canceled"}},
	}); err != nil {
		t.Fatal(err)
	}
	j2 := e.launchFrom(tmpl)
	if _, err := e.st.CancelJob(e.ctx, j2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SendNotifications(e.ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ = e.st.ListJobDeliveries(e.ctx, j2.ID)
	if len(ds) != 1 || ds[0].Status != store.DeliveryPending || ds[0].Attempts != 1 || !strings.Contains(ds[0].LastError, "503") ||
		!ds[0].NextAttemptAt.After(time.Now()) {
		t.Fatalf("after a 503: %+v", ds)
	}
	hook.status = 404
	if _, err := e.pool.Exec(e.ctx, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE id = $1`, ds[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SendNotifications(e.ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ = e.st.ListJobDeliveries(e.ctx, j2.ID)
	if ds[0].Status != store.DeliveryFailed || ds[0].Attempts != 2 || !strings.Contains(ds[0].LastError, "404") {
		t.Errorf("after a 404: %+v", ds[0])
	}
}

func (e *env) launchFrom(tmpl store.JobTemplate) store.Job {
	e.t.Helper()
	cfg, err := e.st.ResolveJobConfig(e.ctx, tmpl, store.LaunchOverrides{})
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.st.CreateJob(e.ctx, store.NewJob{TemplateID: &tmpl.ID, Name: tmpl.Name, LaunchType: "api", Config: cfg})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// backdate gives job j the ID it would have had if created days ago, with
// that day's partition, and returns it.
func (e *env) backdate(j store.Job, days int) store.Job {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `SELECT ensure_job_partitions(
		(now() AT TIME ZONE 'UTC')::date - $1::int, (now() AT TIME ZONE 'UTC')::date - $1::int)`, days); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `
		UPDATE jobs SET id = uuidv7(make_interval(days => -$2::int)), created_at = now() - make_interval(days => $2::int)
		WHERE id = $1 RETURNING id`, j.ID, days).Scan(&j.ID); err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) finish(j store.Job, daysAgo int) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status = 'successful', finished_at = now() - make_interval(days => $2::int) WHERE id = $1`,
		j.ID, daysAgo); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) output(j store.Job) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_events WHERE job_id = $1`, j.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) partitionExists(daysAgo int) bool {
	e.t.Helper()
	var ok bool
	if err := e.pool.QueryRow(e.ctx, `SELECT to_regclass('job_events_p' || to_char((now() AT TIME ZONE 'UTC')::date - $1::int, 'YYYYMMDD')) IS NOT NULL`,
		daysAgo).Scan(&ok); err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func TestCleanUpDropsExpiredDays(t *testing.T) {
	e := newEnv(t)
	e.m.Config.JobRetention = 30 * 24 * time.Hour

	// A day whose jobs all finished long enough ago goes, with its output.
	old := e.backdate(e.launch(60), 40)
	e.finish(old, 39)
	// A day with a job that finished recently stays, all of it.
	mixedOld, mixedNew := e.backdate(e.launch(60), 45), e.backdate(e.launch(60), 45)
	e.finish(mixedOld, 44)
	e.finish(mixedNew, 1)
	// So does a day with a job that hasn't finished, whatever its age.
	running := e.backdate(e.launch(60), 60)
	// And anything recent.
	recent := e.launch(60)
	e.finish(recent, 0)

	for _, j := range []store.Job{old, mixedOld, running} {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO job_events (job_id, seq, stdout) VALUES ($1, 1, 'hi')`, j.ID); err != nil {
			t.Fatal(err)
		}
	}

	if err := e.m.CleanUp(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetJob(e.ctx, old.ID); err != store.ErrNotFound {
		t.Errorf("old job: %v, want deleted", err)
	}
	if e.partitionExists(40) {
		t.Error("the old day's partition was not dropped")
	}
	for _, j := range []store.Job{mixedOld, mixedNew, running, recent} {
		e.job(j.ID)
	}
	if e.output(mixedOld) != 1 || e.output(running) != 1 {
		t.Error("output of a kept day was deleted")
	}
	if !e.partitionExists(45) || !e.partitionExists(60) {
		t.Error("a kept day's partition was dropped")
	}
}

func TestCleanUpFinishesInterruptedDays(t *testing.T) {
	e := newEnv(t)
	e.m.Config.JobRetention = 30 * 24 * time.Hour
	j := e.backdate(e.launch(60), 50)
	e.finish(j, 49)
	// A run that dropped the day's partition, but not yet its jobs.
	part := `job_events_p` + time.Now().UTC().AddDate(0, 0, -50).Format("20060102")
	for _, sql := range []string{`ALTER TABLE job_events DETACH PARTITION ` + part, `DROP TABLE ` + part} {
		if _, err := e.pool.Exec(e.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.m.CleanUp(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetJob(e.ctx, j.ID); err != store.ErrNotFound {
		t.Errorf("job: %v, want deleted", err)
	}
}

func TestCleanUpCreatesPartitionsAhead(t *testing.T) {
	e := newEnv(t)
	if err := e.m.CleanUp(e.ctx); err != nil {
		t.Fatal(err)
	}
	ahead, err := e.st.JobPartitionsAhead(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ahead < partitionsAhead {
		t.Errorf("partitions ahead = %d, want %d", ahead, partitionsAhead)
	}
}
