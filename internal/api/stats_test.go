package api_test

import (
	"context"
	"testing"
	"time"

	"dispatch-ops/internal/testdb"
)

func TestDashboardStats(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	carol, carolID := newUser(t, srv, admin)
	bob, _ := newUser(t, srv, admin)
	pool, _ := testdb.Open(t, "api")
	ctx := context.Background()

	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	newTemplate := func() map[string]any {
		return admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
			"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
			"command": []string{"true"}, "inventoryId": inv["id"],
		}, 201)
	}
	jt, other := newTemplate(), newTemplate()
	grant(admin, map[string]any{"role": "read", "resourceType": "job_template", "resourceId": jt["id"], "userId": carolID})

	// launch launches a job of tmpl and rewrites its history: created ago
	// before now, then (if status is final) ran for runFor after a 5s wait.
	now := time.Now()
	launch := func(tmpl map[string]any, status string, ago, runFor time.Duration) string {
		t.Helper()
		id := admin.mustDo("POST", "/api/v1/job-templates/"+tmpl["id"].(string)+"/launch", nil, 201)["id"].(string)
		created := now.Add(-ago)
		var started, finished *time.Time
		if status != "pending" {
			s := created.Add(5 * time.Second)
			f := s.Add(runFor)
			started, finished = &s, &f
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status = $2, created_at = $3, started_at = $4, finished_at = $5 WHERE id = $1`,
			id, status, created, started, finished); err != nil {
			t.Fatal(err)
		}
		return id
	}
	launch(jt, "successful", time.Hour, time.Minute)
	failed := launch(jt, "failed", 2*time.Hour, 2*time.Minute)
	errored := launch(jt, "error", 3*time.Hour, 0)
	launch(jt, "pending", 0, 0)
	launch(jt, "successful", 8*24*time.Hour, time.Minute) // the previous week
	launch(other, "successful", time.Hour, time.Minute)   // not carol's

	events := []struct {
		job, typ, host, data string
	}{
		{failed, "host_failed", "web1", `{}`},
		{failed, "host_failed", "web2", `{"ignored": true}`},
		{failed, "host_ok", "web3", `{}`},
		{errored, "host_unreachable", "web1", `{}`},
	}
	for i, e := range events {
		if _, err := pool.Exec(ctx, `INSERT INTO job_events (job_id, seq, event_type, host_name, data) VALUES ($1, $2, $3, $4, $5)`,
			e.job, i, e.typ, e.host, e.data); err != nil {
			t.Fatal(err)
		}
	}

	// carol sees only jt's jobs.
	st := carol.mustDo("GET", "/api/v1/stats?window=7d", nil, 200)
	totals := st["totals"].(map[string]any)
	want := map[string]float64{"total": 4, "successful": 1, "failed": 1, "error": 1, "canceled": 0, "active": 1}
	for k, v := range want {
		if totals[k] != v {
			t.Errorf("totals.%s = %v, want %v", k, totals[k], v)
		}
	}
	if r := totals["successRate"].(float64); r < 0.333 || r > 0.334 {
		t.Errorf("successRate = %v, want 1/3", r)
	}
	// Run times are 60s, 120s and (for the error) 0s.
	if p50 := totals["p50DurationSeconds"]; p50 != 60.0 {
		t.Errorf("p50DurationSeconds = %v, want 60", p50)
	}
	if w := totals["p50QueueWaitSeconds"]; w != 5.0 {
		t.Errorf("p50QueueWaitSeconds = %v, want 5", w)
	}
	if prev := st["previousTotals"].(map[string]any)["total"]; prev != 1.0 {
		t.Errorf("previousTotals.total = %v, want 1", prev)
	}
	if st["queue"] != nil || st["notifications"] != nil {
		t.Errorf("non-auditor got queue %v, notifications %v", st["queue"], st["notifications"])
	}

	series := st["series"].([]any)
	if len(series) != 7 || st["bucket"] != "day" {
		t.Errorf("series has %d %v buckets, want 7 days", len(series), st["bucket"])
	}
	var sum float64
	for _, b := range series {
		for _, k := range []string{"successful", "failed", "error", "canceled", "active"} {
			sum += b.(map[string]any)[k].(float64)
		}
	}
	if sum != 4 {
		t.Errorf("series counts %v jobs, want 4", sum)
	}

	templates := st["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("templates = %v, want only jt", templates)
	}
	tmpl := templates[0].(map[string]any)
	if tmpl["id"] != jt["id"] || tmpl["runs"] != 4.0 || tmpl["failures"] != 2.0 {
		t.Errorf("template stats = %v", tmpl)
	}
	recent := tmpl["recent"].([]any)
	if len(recent) != 5 || recent[0] != "pending" || recent[4] != "successful" {
		t.Errorf("recent = %v, want 5 statuses, newest (pending) first", recent)
	}

	hosts := st["problemHosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("problemHosts = %v, want only web1 (web2's failure was ignored)", hosts)
	}
	web1 := hosts[0].(map[string]any)
	if web1["hostName"] != "web1" || web1["failedJobs"] != 1.0 || web1["unreachableJobs"] != 1.0 || web1["lastJobId"] != errored {
		t.Errorf("web1 = %v", web1)
	}

	// Hourly buckets for a day, aligned to the requested zone.
	day := carol.mustDo("GET", "/api/v1/stats?window=24h", nil, 200)
	if n := len(day["series"].([]any)); n != 24 || day["bucket"] != "hour" {
		t.Errorf("24h series has %d %v buckets", n, day["bucket"])
	}
	ny := carol.mustDo("GET", "/api/v1/stats?window=30d&tz=America/New_York", nil, 200)
	loc, _ := time.LoadLocation("America/New_York")
	first, _ := time.Parse(time.RFC3339, ny["series"].([]any)[0].(map[string]any)["start"].(string))
	if h := first.In(loc); h.Hour() != 0 || h.Minute() != 0 {
		t.Errorf("first New York day bucket starts at %v", h)
	}
	if n := len(ny["series"].([]any)); n != 30 {
		t.Errorf("30d series has %d buckets", n)
	}
	carol.mustDo("GET", "/api/v1/stats?tz=Not/AZone", nil, 400)
	carol.mustDo("GET", "/api/v1/stats?window=1y", nil, 400)

	// Without roles, bob sees nothing.
	none := bob.mustDo("GET", "/api/v1/stats", nil, 200)
	if n := none["totals"].(map[string]any)["total"]; n != 0.0 || len(none["templates"].([]any)) != 0 || len(none["problemHosts"].([]any)) != 0 {
		t.Errorf("bob's stats = %v", none)
	}

	// Admins see every job, the queue and notifications.
	all := admin.mustDo("GET", "/api/v1/stats", nil, 200)
	if n := all["totals"].(map[string]any)["total"].(float64); n < 5 {
		t.Errorf("admin total = %v, want at least 5", n)
	}
	q, ok := all["queue"].(map[string]any)
	if !ok || q["pending"].(float64) < 1 {
		t.Errorf("admin queue = %v", all["queue"])
	}
	if _, ok := all["notifications"].(map[string]any); !ok {
		t.Errorf("admin notifications = %v", all["notifications"])
	}
}
