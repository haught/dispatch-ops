package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/store"
)

func TestSchedules(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	carol, carolID := newUser(t, srv, admin)
	dave, _ := newUser(t, srv, admin)

	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "allowOverrides": []string{"limit"},
	}, 201)
	jtID := jt["id"].(string)

	// Preview checks the expression and lists run times.
	prev := admin.mustDo("GET", "/api/v1/schedule-preview?cron=0+3+*+*+*&timezone=Europe/Berlin&count=3", nil, 200)
	if runs := prev["runs"].([]any); len(runs) != 3 {
		t.Errorf("preview = %v", prev)
	}
	admin.mustDo("GET", "/api/v1/schedule-preview?cron=61+*+*+*+*", nil, 400)
	admin.mustDo("GET", "/api/v1/schedule-preview?cron=@daily&timezone=Nowhere/Land", nil, 400)

	// Creating needs execute on the template.
	body := map[string]any{
		"name": "nightly", "jobTemplateId": jtID, "cron": "0 3 * * *", "timezone": "America/New_York",
		"overrides": map[string]any{"limit": "web*"},
	}
	bob.mustDo("POST", "/api/v1/schedules", body, 400) // can't see the template
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jtID, "userId": bobID})
	bob.mustDo("POST", "/api/v1/schedules", map[string]any{
		"name": "x", "jobTemplateId": jtID, "cron": "@hourly", "overrides": map[string]any{"extraVars": map[string]any{"a": 1}},
	}, 400) // the template doesn't allow overriding extra vars
	bob.mustDo("POST", "/api/v1/schedules", map[string]any{"name": "x", "jobTemplateId": jtID, "cron": "@every 10s"}, 400)
	sc := bob.mustDo("POST", "/api/v1/schedules", body, 201)
	scPath := "/api/v1/schedules/" + sc["id"].(string)
	bob.mustDo("POST", "/api/v1/schedules", body, 409)

	next, err := time.Parse(time.RFC3339, sc["nextRunAt"].(string))
	if err != nil || !next.After(time.Now()) {
		t.Errorf("nextRunAt = %v", sc["nextRunAt"])
	}
	if h := next.In(mustLoad(t, "America/New_York")).Hour(); h != 3 {
		t.Errorf("next run %v is not 03:00 in New York", next)
	}
	if sc["enabled"] != true || sc["jobTemplateName"] != jt["name"] || sc["createdBy"].(map[string]any)["id"] != bobID ||
		sc["overrides"].(map[string]any)["limit"] != "web*" {
		t.Errorf("schedule = %v", sc)
	}

	// Reading needs a role on the template.
	carol.mustDo("GET", scPath, nil, 404)
	if got := carol.mustDo("GET", "/api/v1/schedules", nil, 200); got["total"].(float64) != 0 {
		t.Errorf("carol lists schedules: %v", got)
	}
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jtID, "userId": carolID})
	carol.mustDo("GET", scPath, nil, 200)
	if got := carol.mustDo("GET", "/api/v1/schedules?jobTemplateId="+jtID, nil, 200); got["total"].(float64) != 1 {
		t.Errorf("carol lists %v", got)
	}
	// Only the creator (with execute) and template admins may change it.
	carol.mustDo("PATCH", scPath, map[string]any{"enabled": false}, 403)
	carol.mustDo("DELETE", scPath, nil, 403)
	dave.mustDo("PATCH", scPath, map[string]any{"enabled": false}, 404)

	off := bob.mustDo("PATCH", scPath, map[string]any{"enabled": false}, 200)
	if off["enabled"] != false || off["nextRunAt"] != nil {
		t.Errorf("disabled schedule = %v", off)
	}
	// Turning it back on makes the admin its owner: it now runs with their
	// access, and bob, no longer its owner, can't change it.
	on := admin.mustDo("PATCH", scPath, map[string]any{"enabled": true, "cron": "*/5 * * * *", "timezone": "UTC"}, 200)
	if on["nextRunAt"] == nil || on["cron"] != "*/5 * * * *" || on["createdBy"].(map[string]any)["id"] == bobID {
		t.Errorf("re-enabled schedule = %v", on)
	}
	// Changing only the description keeps the planned run.
	same := admin.mustDo("PATCH", scPath, map[string]any{"description": "every five minutes"}, 200)
	if same["nextRunAt"] != on["nextRunAt"] {
		t.Errorf("nextRunAt changed from %v to %v", on["nextRunAt"], same["nextRunAt"])
	}
	admin.mustDo("PATCH", scPath, map[string]any{"cron": "bogus"}, 400)

	// The audit log records schedule changes.
	audit := admin.mustDo("GET", "/api/v1/audit-log?resourceType=schedule&resourceId="+sc["id"].(string), nil, 200)
	if audit["total"].(float64) != 4 {
		t.Errorf("audit entries = %v", audit["total"])
	}

	bob.mustDo("DELETE", scPath, nil, 403)
	admin.mustDo("DELETE", scPath, nil, 204)
	bob.mustDo("GET", scPath, nil, 404)
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestRelaunch(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"echo", "hi"}, "allowOverrides": []string{"limit"}})
	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)

	again := e.c.mustDo("POST", "/api/v1/jobs/"+jobID+"/relaunch", nil, 201)
	if again["relaunchOf"] != jobID || again["launchType"] != "relaunch" || again["status"] != "pending" ||
		jsonOf(again["config"]) != jsonOf(job["config"]) {
		t.Errorf("relaunched = %v", again)
	}
	// Failed hosts only apply to finished Ansible jobs.
	e.c.mustDo("POST", "/api/v1/jobs/"+jobID+"/relaunch", map[string]any{"hosts": "failed"}, 400)

	// An Ansible job where web2 failed and db1 was unreachable.
	ctx := context.Background()
	ans, err := e.st.CreateJob(ctx, store.NewJob{Name: "playbook", LaunchType: "api", Config: store.JobConfig{
		JobType: "ansible", TimeoutSeconds: 60, Limit: "all",
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: uuid.MustParse(e.ee), Image: "local/test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ansPath := "/api/v1/jobs/" + ans.ID.String()
	e.claim(ans.ID.String())
	e.c.mustDo("POST", ansPath+"/relaunch", map[string]any{"hosts": "failed"}, 409) // not finished
	if _, err := e.st.StartJob(ctx, ans.ID, "test"); err != nil {
		t.Fatal(err)
	}
	host := func(s string) *string { return &s }
	if _, err := e.st.AppendJobEvents(ctx, ans.ID, []store.JobEvent{
		{Seq: 1, Time: time.Now(), Type: "host_ok", HostName: host("web1")},
		{Seq: 2, Time: time.Now(), Type: "host_failed", HostName: host("web2")},
		{Seq: 3, Time: time.Now(), Type: "host_unreachable", HostName: host("db1")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishJob(ctx, ans.ID, store.JobFailed, new(2), "", ""); err != nil {
		t.Fatal(err)
	}
	failed := e.c.mustDo("POST", ansPath+"/relaunch", map[string]any{"hosts": "failed"}, 201)
	if got := failed["config"].(map[string]any)["limit"]; got != "db1,web2" {
		t.Errorf("failed-hosts limit = %v", got)
	}
}

func TestRelaunchNeedsExecute(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	other := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "inventoryId": inv["id"], "allowOverrides": []string{"inventory"},
	}, 201)
	jtPath := "/api/v1/job-templates/" + jt["id"].(string)
	job := admin.mustDo("POST", jtPath+"/launch", map[string]any{"inventoryId": other["id"]}, 201)
	relaunch := "/api/v1/jobs/" + job["id"].(string) + "/relaunch"

	grant(admin, map[string]any{"role": "read", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
	bob.mustDo("POST", relaunch, nil, 403)
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
	// The job overrode the inventory with one bob can't use; as at launch,
	// it is reported as missing.
	bob.mustDo("POST", relaunch, nil, 400)
	grant(admin, map[string]any{"role": "use", "resourceType": "inventory", "resourceId": other["id"], "userId": bobID})
	bob.mustDo("POST", relaunch, nil, 201)
}

func TestNotificationTargets(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)

	var hits []*http.Request
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits = append(hits, r)
		if strings.HasSuffix(r.URL.Path, "/broken") {
			w.WriteHeader(500)
		}
	}))
	defer hook.Close()

	// Secrets are write-only.
	target := admin.mustDo("POST", "/api/v1/notification-targets", map[string]any{
		"name": unique("hook"), "kind": "webhook", "config": map[string]any{"url": hook.URL + "/ok"},
		"secrets": map[string]string{"authorization": "Bearer top-secret"},
	}, 201)
	if strings.Contains(jsonOf(target), "top-secret") || strs(target["secretFields"])[0] != "authorization" {
		t.Errorf("target = %v", target)
	}
	targetPath := "/api/v1/notification-targets/" + target["id"].(string)
	admin.mustDo("POST", "/api/v1/notification-targets", map[string]any{
		"name": unique("slack"), "kind": "slack", "config": map[string]any{"url": "https://hooks.slack.com/x"},
	}, 400)
	admin.mustDo("POST", "/api/v1/notification-targets", map[string]any{
		"name": target["name"], "kind": "slack", "secrets": map[string]string{"webhookUrl": "https://hooks.slack.com/x"},
	}, 409)
	chat := admin.mustDo("POST", "/api/v1/notification-targets", map[string]any{
		"name": unique("chat"), "kind": "googlechat",
		"secrets": map[string]string{"webhookUrl": "https://chat.googleapis.com/v1/spaces/A/messages?key=k&token=chat-secret"},
	}, 201)
	if strings.Contains(jsonOf(chat), "chat-secret") || strs(chat["secretFields"])[0] != "webhookUrl" {
		t.Errorf("google chat target = %v", chat)
	}
	admin.mustDo("DELETE", "/api/v1/notification-targets/"+chat["id"].(string), nil, 204)

	// Everyone may list targets, but only admins see their config, and
	// manage them.
	got := bob.mustDo("GET", targetPath, nil, 200)
	if len(got["config"].(map[string]any)) != 0 {
		t.Errorf("bob sees config %v", got["config"])
	}
	bob.mustDo("POST", "/api/v1/notification-targets", map[string]any{"name": unique("x"), "kind": "webhook", "config": map[string]any{"url": "https://x"}}, 403)
	bob.mustDo("PATCH", targetPath, map[string]any{"description": "x"}, 403)
	bob.mustDo("POST", targetPath+"/test", nil, 403)

	// Test sends at once and reports the result.
	if res := admin.mustDo("POST", targetPath+"/test", nil, 200); res["ok"] != true {
		t.Errorf("test = %v", res)
	}
	if len(hits) != 1 || hits[0].Header.Get("Authorization") != "Bearer top-secret" || hits[0].Header.Get("X-Dispatch-Event") != "test" {
		t.Fatalf("webhook hits = %v", hits)
	}
	upd := admin.mustDo("PATCH", targetPath, map[string]any{
		"config": map[string]any{"url": hook.URL + "/broken"}, "secrets": map[string]string{"signingSecret": "k"},
	}, 200)
	if fields := strs(upd["secretFields"]); len(fields) != 2 {
		t.Errorf("secrets after merge = %v", fields)
	}
	if res := admin.mustDo("POST", targetPath+"/test", nil, 200); res["ok"] != false || !strings.Contains(res["error"].(string), "500") {
		t.Errorf("test against a broken URL = %v", res)
	}
	if hits[1].Header.Get("Authorization") != "Bearer top-secret" || hits[1].Header.Get("X-Dispatch-Signature") == "" {
		t.Errorf("merged secrets not used: %v", hits[1].Header)
	}

	// Attaching targets to a template needs admin on it.
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"], "command": []string{"true"},
	}, 201)
	jtNotif := "/api/v1/job-templates/" + jt["id"].(string) + "/notifications"
	set := map[string]any{"items": []any{map[string]any{"targetId": target["id"], "events": []string{"succeeded", "failed"}}}}
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
	bob.mustDo("PUT", jtNotif, set, 403)
	bob.mustDo("GET", jtNotif, nil, 200)
	admin.mustDo("PUT", jtNotif, map[string]any{"items": []any{map[string]any{"targetId": uuid.NewString(), "events": []string{"failed"}}}}, 400)
	admin.mustDo("PUT", jtNotif, map[string]any{"items": []any{map[string]any{"targetId": target["id"], "events": []string{}}}}, 400)
	list := admin.mustDo("PUT", jtNotif, set, 200)
	items := list["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["targetName"] != target["name"] {
		t.Errorf("template notifications = %v", list)
	}

	// A job ending queues its notifications, which the job lists.
	job := bob.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch", nil, 201)
	bob.mustDo("POST", "/api/v1/jobs/"+job["id"].(string)+"/cancel", nil, 202)
	if got := bob.mustDo("GET", "/api/v1/jobs/"+job["id"].(string)+"/notifications", nil, 200); len(got["items"].([]any)) != 0 {
		t.Errorf("canceled job notified for an event not chosen: %v", got)
	}
	admin.mustDo("PUT", jtNotif, map[string]any{"items": []any{map[string]any{"targetId": target["id"], "events": []string{"canceled"}}}}, 200)
	job = bob.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch", nil, 201)
	bob.mustDo("POST", "/api/v1/jobs/"+job["id"].(string)+"/cancel", nil, 202)
	deliveries := bob.mustDo("GET", "/api/v1/jobs/"+job["id"].(string)+"/notifications", nil, 200)["items"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %v", deliveries)
	}
	if d := deliveries[0].(map[string]any); d["event"] != "canceled" || d["status"] != "pending" || d["nextAttemptAt"] == nil {
		t.Errorf("delivery = %v", d)
	}

	// Deleting the target detaches it.
	admin.mustDo("DELETE", targetPath, nil, 204)
	if got := admin.mustDo("GET", jtNotif, nil, 200); len(got["items"].([]any)) != 0 {
		t.Errorf("deleted target still attached: %v", got)
	}
}

func TestHTTPMetrics(t *testing.T) {
	srv, _, _ := testServer(t)
	anon := &client{t: t, base: srv.URL}
	health := metrics.HTTPRequests.WithLabelValues("getHealth", "GET", "200")
	unmatched := metrics.HTTPRequests.WithLabelValues(metrics.OpUnmatched, "GET", "404")
	before, beforeUnmatched := testutil.ToFloat64(health), testutil.ToFloat64(unmatched)
	anon.do("GET", "/api/v1/health", nil)
	anon.do("GET", "/api/v1/nope", nil)
	if got := testutil.ToFloat64(health) - before; got != 1 {
		t.Errorf("getHealth requests counted %v times", got)
	}
	if got := testutil.ToFloat64(unmatched) - beforeUnmatched; got != 1 {
		t.Errorf("unmatched requests counted %v times", got)
	}
}
