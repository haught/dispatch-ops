package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
)

// A launcher can't make the job hand over its credentials through a limit
// that names a file (Ansible reads it and prints each line as a warning),
// or through extra vars Ansible connects with or evaluates.
func TestLaunchOverridesCantReachCredentials(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "allowOverrides": []string{"limit", "extraVars"},
	}, 201)
	jtPath := "/api/v1/job-templates/" + jt["id"].(string)
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})

	for _, limit := range []string{
		"@/etc/passwd", "@/runner/secrets/credentials.json", "web,@/etc/passwd", "web, @/etc/passwd", "web:@/etc/passwd",
	} {
		bob.mustDo("POST", jtPath+"/launch", map[string]any{"limit": limit}, 400)
	}
	for _, vars := range []map[string]any{
		{"ansible_ssh_common_args": `-o ProxyCommand="sh -c 'cat /runner/secrets/credentials.json'"`},
		{"ansible_become_exe": "sh"},
		{"x": "{{ lookup('file', '/runner/secrets/credentials.json') }}"},
		{"x": map[string]any{"y": []any{"{% raw %}"}}},
		{"{{ x }}": "y"},
	} {
		bob.mustDo("POST", jtPath+"/launch", map[string]any{"extraVars": vars}, 400)
	}
	bob.mustDo("POST", jtPath+"/launch", map[string]any{"limit": "web,db:&prod", "extraVars": map[string]any{"color": "blue"}}, 201)

	// Schedules store overrides; the same rules apply.
	sched := map[string]any{"name": unique("s"), "jobTemplateId": jt["id"], "cron": "0 3 * * *"}
	sched["overrides"] = map[string]any{"limit": "@/etc/passwd"}
	bob.mustDo("POST", "/api/v1/schedules", sched, 400)
	sched["overrides"] = map[string]any{"extraVars": map[string]any{"ansible_host": "evil.example"}}
	bob.mustDo("POST", "/api/v1/schedules", sched, 400)

	// The template's own limit, too.
	admin.mustDo("PATCH", jtPath, map[string]any{"limit": "@/etc/passwd"}, 400)
	admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "limit": "all,@/etc/passwd",
	}, 400)

	// Host names end up in limits (relaunch on failed hosts).
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	hosts := "/api/v1/inventories/" + inv["id"].(string) + "/hosts"
	for _, name := range []string{"@/etc/passwd", "a,b", "a:@b"} {
		admin.mustDo("POST", hosts, map[string]any{"name": name}, 400)
	}
	h := admin.mustDo("POST", hosts, map[string]any{"name": "web1"}, 201)
	admin.mustDo("PATCH", "/api/v1/hosts/"+h["id"].(string), map[string]any{"name": "@/etc/passwd"}, 400)
}

// A job's events, which the job itself writes, can name hosts that would
// make a failed-hosts limit read a file; and a job stored with such a limit
// before limits were checked can't be relaunched.
func TestRelaunchRefusesFileLimits(t *testing.T) {
	e := newJobEnv(t)
	ctx := context.Background()
	newJob := func(limit string) store.Job {
		j, err := e.st.CreateJob(ctx, store.NewJob{Name: "playbook", LaunchType: "api", Config: store.JobConfig{
			JobType: "ansible", TimeoutSeconds: 60, Limit: limit,
			ExecutionEnvironment: store.JobExecutionEnvironment{ID: uuid.MustParse(e.ee), Image: "local/test"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		e.claim(j.ID.String())
		if _, err := e.st.StartJob(ctx, j.ID, "test"); err != nil {
			t.Fatal(err)
		}
		return j
	}

	j := newJob("all")
	host := "@/runner/secrets/credentials.json"
	if _, err := e.st.AppendJobEvents(ctx, j.ID, []store.JobEvent{
		{Seq: 1, Time: time.Now(), Type: "host_failed", HostName: &host},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishJob(ctx, j.ID, store.JobFailed, new(2), "", ""); err != nil {
		t.Fatal(err)
	}
	e.c.mustDo("POST", "/api/v1/jobs/"+j.ID.String()+"/relaunch", map[string]any{"hosts": "failed"}, 400)

	old := newJob("@/etc/passwd")
	if err := e.st.FinishJob(ctx, old.ID, store.JobSuccessful, new(0), "", ""); err != nil {
		t.Fatal(err)
	}
	e.c.mustDo("POST", "/api/v1/jobs/"+old.ID.String()+"/relaunch", nil, 400)
}

// Roles that change what a job runs, or where its secrets go, need access
// to the credentials involved.
func TestDelegatedAdminsNeedCredentialUse(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	cred := admin.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("cred"), "kind": "generic", "inputs": map[string]string{"TOKEN": "s3cret"},
	}, 201)

	// Template admin: harmless changes are fine; changing what runs needs
	// use on the template's credentials.
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "credentialIds": []any{cred["id"]},
	}, 201)
	jtPath := "/api/v1/job-templates/" + jt["id"].(string)
	grant(admin, map[string]any{"role": "admin", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
	bob.mustDo("PATCH", jtPath, map[string]any{"description": "bob's", "limit": "web", "timeoutSeconds": 60}, 200)
	bob.mustDo("PATCH", jtPath, map[string]any{"command": []string{"true"}}, 200) // unchanged
	bob.mustDo("PATCH", jtPath, map[string]any{"command": []string{"sh", "-c", "cat $DISPATCH_CREDENTIALS"}}, 403)
	bob.mustDo("PATCH", jtPath, map[string]any{"extraVars": map[string]any{"x": 1}}, 403)
	bob.mustDo("PATCH", jtPath, map[string]any{"allowOverrides": []string{"command"}}, 403)
	other := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/other"}, 201)
	bob.mustDo("PATCH", jtPath, map[string]any{"executionEnvironmentId": other["id"]}, 403)
	// Dropping the credential, or holding use on it, allows the change.
	grant(admin, map[string]any{"role": "use", "resourceType": "credential", "resourceId": cred["id"], "userId": bobID})
	bob.mustDo("PATCH", jtPath, map[string]any{"command": []string{"echo", "hi"}}, 200)

	// Project admin: moving the project to another server with its
	// credential needs use on the credential; another repo on the same
	// server doesn't.
	scm := admin.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("scm"), "kind": "scm", "inputs": map[string]string{"username": "u", "password": "tok"},
	}, 201)
	p := admin.mustDo("POST", "/api/v1/projects", map[string]any{
		"name": unique("p"), "scmUrl": "https://git.example.com/org/repo.git", "credentialId": scm["id"],
	}, 201)
	pPath := "/api/v1/projects/" + p["id"].(string)
	grant(admin, map[string]any{"role": "admin", "resourceType": "project", "resourceId": p["id"], "userId": bobID})
	bob.mustDo("PATCH", pPath, map[string]any{"scmUrl": "https://GIT.example.com:443/org/other.git"}, 200)
	for _, u := range []string{
		"https://attacker.example/x.git", "http://git.example.com/org/repo.git", "https://git.example.com:8443/org/repo.git",
		"git@attacker.example:x.git",
	} {
		bob.mustDo("PATCH", pPath, map[string]any{"scmUrl": u}, 403)
	}
	bob.mustDo("PATCH", pPath, map[string]any{"scmUrl": "https://attacker.example/x.git", "credentialId": nil}, 200)

	// Credential admin: changing where a secret is sent needs it again.
	reg := admin.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("reg"), "kind": "registry", "inputs": map[string]string{"server": "ghcr.io", "username": "bot", "password": "pw"},
	}, 201)
	regPath := "/api/v1/credentials/" + reg["id"].(string)
	grant(admin, map[string]any{"role": "admin", "resourceType": "credential", "resourceId": reg["id"], "userId": bobID})
	bob.mustDo("PATCH", regPath, map[string]any{"inputs": map[string]string{"username": "other"}}, 200)
	bob.mustDo("PATCH", regPath, map[string]any{"inputs": map[string]string{"server": "evil.example"}}, 400)
	bob.mustDo("PATCH", regPath, map[string]any{"inputs": map[string]string{"server": "evil.example", "password": "new"}}, 200)
}
