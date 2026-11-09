package api_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
)

// syncEnv is a jobEnv with an inventory synced from a project.
type syncEnv struct {
	*jobEnv
	inv     string
	invPath string
}

func newSyncEnv(t *testing.T, files map[string]string) (*syncEnv, string) {
	t.Helper()
	if _, err := exec.LookPath("ansible-inventory"); err != nil {
		t.Skip("ansible-inventory is not installed")
	}
	e := newJobEnv(t)
	url, _ := gitRepo(t, files)
	proj := e.project(url, "main")
	inv := e.c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("synced")}, 201)["id"].(string)
	return &syncEnv{jobEnv: e, inv: inv, invPath: "/api/v1/inventories/" + inv}, proj
}

// sync launches a sync of the inventory, runs it and returns the job.
func (e *syncEnv) sync() map[string]any {
	e.t.Helper()
	job := e.c.mustDo("POST", e.invPath+"/sync", nil, 201)
	return e.run(job["id"].(string))
}

// log returns all of the job's output, the runner's own messages included.
func (e *syncEnv) log(jobID string) string {
	e.t.Helper()
	var lines []string
	for _, it := range e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?limit=5000", nil, 200)["items"].([]any) {
		if s, ok := it.(map[string]any)["stdout"].(string); ok {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}

func (e *syncEnv) hostNames() []string {
	e.t.Helper()
	var out []string
	for _, h := range e.c.mustDo("GET", e.invPath+"/hosts?limit=500", nil, 200)["items"].([]any) {
		out = append(out, h.(map[string]any)["name"].(string))
	}
	sort.Strings(out)
	return out
}

func (e *syncEnv) hostID(name string) string {
	e.t.Helper()
	for _, h := range e.c.mustDo("GET", e.invPath+"/hosts?search="+name, nil, 200)["items"].([]any) {
		if m := h.(map[string]any); m["name"] == name {
			return m["id"].(string)
		}
	}
	e.t.Fatalf("no host %s", name)
	return ""
}

func TestInventorySync(t *testing.T) {
	e, proj := newSyncEnv(t, map[string]string{
		"inventory/hosts.yml": `all:
  children:
    prod:
      children:
        db-servers:
          hosts:
            db1: {port: 5432}
          vars: {role: database}
    web:
      hosts:
        web1: {color: blue}
        web2:
`,
		// A script inventory whose host name comes from a generic
		// credential, through the environment.
		"dyn.sh": `#!/bin/sh
if [ "$1" = "--list" ]; then
  echo "{\"dyn\": {\"hosts\": [\"$SYNC_TOKEN-host\"], \"vars\": {\"from\": \"script\"}}, \"_meta\": {\"hostvars\": {\"$SYNC_TOKEN-host\": {\"x\": 1}}}}"
else
  echo '{}'
fi
`,
		"broken.yml": "plugin: no.such.plugin\n",
	})

	// No source yet.
	e.c.mustDo("GET", e.invPath+"/source", nil, 404)
	e.c.mustDo("POST", e.invPath+"/sync", nil, 400)
	if inv := e.c.mustDo("GET", e.invPath, nil, 200); inv["source"] != nil {
		t.Fatalf("source = %v", inv["source"])
	}

	// Bad sources.
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "../etc/hosts", "executionEnvironmentId": e.ee}, 400)
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": uuid.NewString(), "sourcePath": "inventory", "executionEnvironmentId": e.ee}, 400)
	ssh := e.c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("ssh"), "kind": "ssh", "inputs": map[string]string{"private_key": "KEY"},
	}, 201)
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{
		"projectId": proj, "sourcePath": "inventory", "executionEnvironmentId": e.ee, "credentialIds": []any{ssh["id"]},
	}, 400)

	// A directory source.
	src := e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "inventory", "executionEnvironmentId": e.ee}, 200)
	if src["sourcePath"] != "inventory" || src["timeoutSeconds"] != 600.0 || src["lastSyncedAt"] != nil {
		t.Errorf("source = %s", jsonOf(src))
	}
	if inv := e.c.mustDo("GET", e.invPath, nil, 200); inv["source"] == nil {
		t.Error("inventory has no source summary")
	}

	// Job templates can't be inventory syncs.
	e.c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "inventory_sync", "executionEnvironmentId": e.ee, "projectId": proj, "entrypoint": "inventory",
	}, 400)

	// One sync at a time.
	pending := e.c.mustDo("POST", e.invPath+"/sync", nil, 201)
	if pending["syncInventoryId"] != e.inv || pending["config"].(map[string]any)["jobType"] != "inventory_sync" {
		t.Fatalf("sync job = %s", jsonOf(pending))
	}
	e.c.mustDo("POST", e.invPath+"/sync", nil, 409)
	job := e.run(pending["id"].(string))
	if job["status"] != "successful" {
		t.Fatalf("sync job = %s\n%s", jsonOf(job), e.log(pending["id"].(string)))
	}
	if out := e.log(pending["id"].(string)); !strings.Contains(out, "hosts 3 added, 0 updated, 0 removed; groups 3 added") {
		t.Errorf("output lacks the summary:\n%s", out)
	}
	if got := e.hostNames(); !slices.Equal(got, []string{"db1", "web1", "web2"}) {
		t.Errorf("hosts = %v", got)
	}

	// The export has the groups, nesting and variables, with group
	// variables on the group (hyphenated names are kept).
	exp := e.c.mustDo("GET", e.invPath+"/export", nil, 200)
	db := exp["db-servers"].(map[string]any)
	if !slices.Equal(strs(db["hosts"]), []string{"db1"}) || db["vars"].(map[string]any)["role"] != "database" {
		t.Errorf("db-servers = %s", jsonOf(db))
	}
	if !slices.Contains(strs(exp["prod"].(map[string]any)["children"]), "db-servers") {
		t.Errorf("prod = %s", jsonOf(exp["prod"]))
	}
	hv := exp["_meta"].(map[string]any)["hostvars"].(map[string]any)
	if hv["web1"].(map[string]any)["color"] != "blue" || hv["db1"].(map[string]any)["port"] != 5432.0 {
		t.Errorf("hostvars = %s", jsonOf(hv))
	}
	if src := e.c.mustDo("GET", e.invPath+"/source", nil, 200); src["lastSyncedAt"] == nil || src["lastJobStatus"] != "successful" {
		t.Errorf("source after sync = %s", jsonOf(src))
	}

	// Hosts and groups can't be edited by hand while synced.
	web1 := e.hostID("web1")
	e.c.mustDo("POST", e.invPath+"/hosts", map[string]any{"name": "manual"}, 409)
	e.c.mustDo("PATCH", "/api/v1/hosts/"+web1, map[string]any{"description": "x"}, 409)
	e.c.mustDo("DELETE", "/api/v1/hosts/"+web1, nil, 409)
	e.c.mustDo("POST", e.invPath+"/groups", map[string]any{"name": "manual"}, 409)
	// The inventory's own variables can.
	e.c.mustDo("PATCH", e.invPath, map[string]any{"variables": map[string]any{"env": "prod"}}, 200)

	// Syncing again keeps hosts' IDs; a relaunch syncs again too.
	again := e.c.mustDo("POST", "/api/v1/jobs/"+pending["id"].(string)+"/relaunch", nil, 201)
	if again["syncInventoryId"] != e.inv || again["launchType"] != "relaunch" {
		t.Fatalf("relaunch = %s", jsonOf(again))
	}
	if job := e.run(again["id"].(string)); job["status"] != "successful" {
		t.Fatalf("relaunched sync = %s", jsonOf(job))
	}
	if !strings.Contains(e.log(again["id"].(string)), "hosts 0 added, 0 updated, 0 removed") {
		t.Errorf("an unchanged resync changed things:\n%s", e.log(again["id"].(string)))
	}
	if e.hostID("web1") != web1 {
		t.Error("a resync gave web1 a new ID")
	}
	// Sync jobs are listed per inventory.
	if n := e.c.mustDo("GET", "/api/v1/jobs?syncInventoryId="+e.inv, nil, 200)["total"]; n != 2.0 {
		t.Errorf("sync jobs = %v", n)
	}

	// A source Ansible can't parse fails the sync and leaves the
	// inventory as it was.
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "broken.yml", "executionEnvironmentId": e.ee}, 200)
	if job := e.sync(); job["status"] != "failed" {
		t.Errorf("broken sync = %s", jsonOf(job))
	}
	if got := e.hostNames(); len(got) != 3 {
		t.Errorf("hosts after a failed sync = %v", got)
	}

	// A script inventory reads a generic credential from the environment;
	// hosts no longer in the source are removed.
	cred := e.c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("token"), "kind": "generic", "inputs": map[string]string{"SYNC_TOKEN": "abc"},
	}, 201)
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{
		"projectId": proj, "sourcePath": "dyn.sh", "executionEnvironmentId": e.ee, "credentialIds": []any{cred["id"]},
	}, 200)
	if job := e.sync(); job["status"] != "successful" {
		t.Fatalf("script sync = %s\n%s", jsonOf(job), e.log(job["id"].(string)))
	}
	if got := e.hostNames(); !slices.Equal(got, []string{"abc-host"}) {
		t.Errorf("hosts after script sync = %v", got)
	}

	// Without a source, hosts can be edited again and are kept.
	e.c.mustDo("DELETE", e.invPath+"/source", nil, 204)
	e.c.mustDo("POST", e.invPath+"/hosts", map[string]any{"name": "manual"}, 201)
	if got := e.hostNames(); !slices.Equal(got, []string{"abc-host", "manual"}) {
		t.Errorf("hosts after removing the source = %v", got)
	}
}

func TestInventorySyncSchedules(t *testing.T) {
	e, proj := newSyncEnv(t, map[string]string{"hosts.yml": "all:\n  hosts:\n    one:\n"})
	ctx := context.Background()

	// A schedule needs a source, and takes no overrides.
	e.c.mustDo("POST", "/api/v1/schedules", map[string]any{"name": "hourly", "inventoryId": e.inv, "cron": "@hourly"}, 400)
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "hosts.yml", "executionEnvironmentId": e.ee}, 200)
	e.c.mustDo("POST", "/api/v1/schedules", map[string]any{
		"name": "hourly", "inventoryId": e.inv, "cron": "@hourly", "overrides": map[string]any{"limit": "x"},
	}, 400)
	e.c.mustDo("POST", "/api/v1/schedules", map[string]any{"name": "hourly", "jobTemplateId": uuid.NewString(), "inventoryId": e.inv, "cron": "@hourly"}, 400)
	sc := e.c.mustDo("POST", "/api/v1/schedules", map[string]any{"name": "hourly", "inventoryId": e.inv, "cron": "@hourly"}, 201)
	if sc["inventoryId"] != e.inv || sc["jobTemplateId"] != nil || sc["inventoryName"] == "" {
		t.Errorf("schedule = %s", jsonOf(sc))
	}
	if n := e.c.mustDo("GET", "/api/v1/schedules?inventoryId="+e.inv, nil, 200)["total"]; n != 1.0 {
		t.Errorf("schedules of the inventory = %v", n)
	}

	// Two due schedules: the first launches a sync, the second finds it
	// still pending.
	invID := uuid.MustParse(e.inv)
	past := time.Now().Add(-time.Minute)
	for _, name := range []string{"a", "b"} {
		if _, err := e.st.CreateSchedule(ctx, store.ScheduleTarget{InventoryID: &invID}, nil, store.ScheduleFields{
			Name: name, Cron: "@daily", Timezone: "UTC", Enabled: true, NextRunAt: &past,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := e.st.RunDueSchedules(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var launched, refused int
	for _, r := range runs {
		switch {
		case r.JobID != nil:
			launched++
			job := e.c.mustDo("GET", "/api/v1/jobs/"+r.JobID.String(), nil, 200)
			if job["launchType"] != "schedule" || job["syncInventoryId"] != e.inv {
				t.Errorf("scheduled sync = %s", jsonOf(job))
			}
		case errors.Is(r.Err, store.ErrSyncActive):
			refused++
		default:
			t.Errorf("run of %s: %v", r.Schedule.Name, r.Err)
		}
	}
	if launched != 1 || refused != 1 {
		t.Errorf("launched %d, refused %d", launched, refused)
	}
}

func TestInventorySyncAccess(t *testing.T) {
	e, proj := newSyncEnv(t, map[string]string{"hosts.yml": "all:\n  hosts:\n    one:\n"})
	e.c.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "hosts.yml", "executionEnvironmentId": e.ee}, 200)
	job := e.c.mustDo("POST", e.invPath+"/sync", nil, 201)["id"].(string)

	srv := &httptest.Server{URL: e.c.base}
	reader, readerID := newUser(t, srv, e.c)
	admin, adminID := newUser(t, srv, e.c)
	grant(e.c, map[string]any{"role": "read", "resourceType": "inventory", "resourceId": e.inv, "userId": readerID})
	grant(e.c, map[string]any{"role": "admin", "resourceType": "inventory", "resourceId": e.inv, "userId": adminID})

	// Read on the inventory shows its sync jobs, but can't sync or cancel.
	reader.mustDo("GET", "/api/v1/jobs/"+job, nil, 200)
	if n := reader.mustDo("GET", "/api/v1/jobs?syncInventoryId="+e.inv, nil, 200)["total"]; n != 1.0 {
		t.Errorf("reader sees %v sync jobs", n)
	}
	reader.mustDo("GET", e.invPath+"/source", nil, 200)
	reader.mustDo("POST", e.invPath+"/sync", nil, 403)
	reader.mustDo("POST", "/api/v1/jobs/"+job+"/cancel", nil, 403)
	reader.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "hosts.yml", "executionEnvironmentId": e.ee}, 403)

	// Admin on the inventory can cancel and sync, but changing the source
	// to a project needs use on it.
	admin.mustDo("POST", "/api/v1/jobs/"+job+"/cancel", nil, 202)
	admin.mustDo("POST", e.invPath+"/sync", nil, 201)
	admin.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": proj, "sourcePath": "other.yml", "executionEnvironmentId": e.ee}, 200)
	other := e.project("file:///nowhere", "main")
	admin.mustDo("PUT", e.invPath+"/source", map[string]any{"projectId": other, "sourcePath": "hosts.yml", "executionEnvironmentId": e.ee}, 400)

	// Someone with no role on the inventory sees nothing.
	stranger, _ := newUser(t, srv, e.c)
	stranger.mustDo("GET", "/api/v1/jobs/"+job, nil, 404)
	stranger.mustDo("GET", e.invPath+"/source", nil, 404)
}

func TestRunnerInventoryReport(t *testing.T) {
	e := newJobEnv(t)
	// Only running inventory_sync jobs may report an inventory.
	jobID := e.launch(map[string]any{"command": []string{"true"}})
	token := e.claim(jobID)
	rc := &client{t: t, base: e.c.base, bearer: token}
	rc.mustDo("GET", "/runner/v1/jobs/"+jobID+"/spec?protocol=3&version=test", nil, 200)
	rc.mustDo("POST", "/runner/v1/jobs/"+jobID+"/inventory", map[string]any{"_meta": map[string]any{"hostvars": map[string]any{}}}, 409)

	// A runner too old for inventory syncs is refused.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	url, _ := gitRepo(t, map[string]string{"hosts.yml": "all:\n  hosts:\n    one:\n"})
	inv := e.c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)["id"].(string)
	e.c.mustDo("PUT", "/api/v1/inventories/"+inv+"/source", map[string]any{
		"projectId": e.project(url, "main"), "sourcePath": "hosts.yml", "executionEnvironmentId": e.ee,
	}, 200)
	sync := e.c.mustDo("POST", "/api/v1/inventories/"+inv+"/sync", nil, 201)["id"].(string)
	old := &client{t: t, base: e.c.base, bearer: e.claim(sync)}
	old.mustDo("GET", "/runner/v1/jobs/"+sync+"/spec?protocol=2&version=old", nil, 400)
	if job := e.c.mustDo("GET", "/api/v1/jobs/"+sync, nil, 200); job["status"] != "error" {
		t.Errorf("sync on an old runner = %s", jsonOf(job))
	}
}

func TestParseAnsibleInventory(t *testing.T) {
	inv, err := store.ParseAnsibleInventory(map[string]any{
		"_meta":     map[string]any{"hostvars": map[string]any{"a": map[string]any{"v": 1.0}, "bad host": map[string]any{}}},
		"all":       map[string]any{"children": []any{"ungrouped", "g", "empty"}, "vars": map[string]any{"ignored": true}},
		"g":         map[string]any{"hosts": []any{"a", "b", "bad host"}, "children": []any{"child", "g", "all"}, "vars": map[string]any{"k": "v"}},
		"ungrouped": map[string]any{"hosts": []any{"c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var hosts, groups []string
	for h := range inv.Hosts {
		hosts = append(hosts, h)
	}
	for g := range inv.Groups {
		groups = append(groups, g)
	}
	sort.Strings(hosts)
	sort.Strings(groups)
	if !slices.Equal(hosts, []string{"a", "b", "c"}) || !slices.Equal(groups, []string{"child", "empty", "g"}) {
		t.Errorf("hosts %v, groups %v", hosts, groups)
	}
	g := inv.Groups["g"]
	if !slices.Equal(g.Hosts, []string{"a", "b"}) || !slices.Equal(g.Children, []string{"child"}) || g.Vars["k"] != "v" {
		t.Errorf("g = %+v", g)
	}
	if inv.Hosts["a"]["v"] != 1.0 || len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "bad host") {
		t.Errorf("a = %v, warnings = %v", inv.Hosts["a"], inv.Warnings)
	}
	if _, err := store.ParseAnsibleInventory(map[string]any{"g": "not an object"}); err == nil {
		t.Error("a malformed group was accepted")
	}
}
