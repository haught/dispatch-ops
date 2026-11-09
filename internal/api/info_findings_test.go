package api_test

import (
	"context"
	"strings"
	"testing"

	"dispatch-ops/internal/store"
)

// I2: a job canceled while its pod starts never hands out its spec, which
// holds the decrypted credentials.
func TestCanceledWaitingJobGetsNoSpec(t *testing.T) {
	e := newJobEnv(t)
	id := e.launch(map[string]any{"command": []string{"true"}})
	token := e.claim(id) // waiting: the pod is starting
	e.c.mustDo("POST", "/api/v1/jobs/"+id+"/cancel", nil, 202)

	r := &client{t: t, base: e.c.base, bearer: token}
	resp, body := r.do("GET", "/runner/v1/jobs/"+id+"/spec?protocol=3&version=x", nil)
	expectStatus(t, resp, body, 409)
	if _, ok := body["credentials"]; ok {
		t.Error("spec returned for a canceled job")
	}
	if job := e.c.mustDo("GET", "/api/v1/jobs/"+id, nil, 200); job["status"] != "canceled" {
		t.Errorf("status = %v, want canceled", job["status"])
	}
}

// I6: a repository URL can't carry a secret; one stored before the rule is
// shown redacted.
func TestScmURLSecrets(t *testing.T) {
	srv, st, username, password := startServer(t)
	admin := login(t, srv.URL, username, password)
	create := func(u string, want int) {
		t.Helper()
		admin.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": u}, want)
	}
	create("https://ghp_secret@github.com/acme/infra.git", 400)
	create("https://user:secret@git.example.com/acme/infra.git", 400)
	create("ssh://git:secret@git.example.com/acme/infra.git", 400)
	create("https://github.com/acme/infra.git", 201)
	create("ssh://git@git.example.com/acme/infra.git", 201)
	create("git@git.example.com:acme/infra.git", 201)

	old, err := st.CreateProject(context.Background(), store.ProjectFields{
		Name: unique("old"), ScmURL: "https://ghp_secret@github.com/acme/infra.git", ScmRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + old.ID.String()
	got := admin.mustDo("GET", path, nil, 200)["scmUrl"].(string)
	if strings.Contains(got, "ghp_secret") || got != "https://REDACTED@github.com/acme/infra.git" {
		t.Errorf("stored token URL shown as %q", got)
	}
	// Saving it back as shown is refused: the token has to move to a credential.
	admin.mustDo("PATCH", path, map[string]any{"scmUrl": got}, 400)
}

// R2-H1: the spec names the launcher's extra vars, so the runner passes
// them to Ansible marked unsafe; the template's own vars aren't in it.
func TestSpecSeparatesOverrideVars(t *testing.T) {
	e := newJobEnv(t)
	jt := e.c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": e.ee,
		"command": []string{"true"}, "extraVars": map[string]any{"base": "{{ 1 + 1 }}"},
		"allowOverrides": []string{"extraVars"},
	}, 201)
	launched := e.c.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch",
		map[string]any{"extraVars": map[string]any{"greeting": "hi"}}, 201)
	id := launched["id"].(string)

	r := &client{t: t, base: e.c.base, bearer: e.claim(id)}
	resp, spec := r.do("GET", "/runner/v1/jobs/"+id+"/spec?protocol=3&version=x", nil)
	expectStatus(t, resp, spec, 200)
	extra, _ := spec["extraVars"].(map[string]any)
	if extra["base"] != "{{ 1 + 1 }}" || extra["greeting"] != "hi" {
		t.Errorf("extraVars = %v, want the template's and the launcher's", extra)
	}
	over, _ := spec["overrideVars"].(map[string]any)
	if len(over) != 1 || over["greeting"] != "hi" {
		t.Errorf("overrideVars = %v, want only the launcher's", spec["overrideVars"])
	}

	// No launcher vars: no overrideVars.
	plain := e.launch(map[string]any{"command": []string{"true"}})
	r = &client{t: t, base: e.c.base, bearer: e.claim(plain)}
	resp, spec = r.do("GET", "/runner/v1/jobs/"+plain+"/spec?protocol=3&version=x", nil)
	expectStatus(t, resp, spec, 200)
	if _, ok := spec["overrideVars"]; ok {
		t.Errorf("overrideVars = %v, want none", spec["overrideVars"])
	}
}

// A job whose template was deleted is relaunched (by a system admin) with
// its stored overrides as they are; those still pass the launch checks,
// so a job stored before a check existed can't bring back what it refuses.
func TestRelaunchDeletedTemplateChecksOverrides(t *testing.T) {
	e := newJobEnv(t)
	ctx := context.Background()
	newJob := func(vars map[string]any) string {
		t.Helper()
		j, err := e.st.CreateJob(ctx, store.NewJob{Name: "orphan", LaunchType: "manual",
			Config:    store.JobConfig{JobType: "container", Command: []string{"true"}, TimeoutSeconds: 60, ExtraVars: vars},
			Overrides: &store.LaunchOverrides{ExtraVars: vars},
		})
		if err != nil {
			t.Fatal(err)
		}
		return "/api/v1/jobs/" + j.ID.String() + "/relaunch"
	}
	for _, vars := range []map[string]any{
		{"x": "{{ lookup('file', '/runner/secrets/credentials.json') }}"},
		{"x": map[string]any{"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256\n6162"}},
		{"ansible_ssh_common_args": "-o ProxyCommand=true"},
	} {
		e.c.mustDo("POST", newJob(vars), nil, 400)
	}
	e.c.mustDo("POST", newJob(map[string]any{"greeting": "hi"}), nil, 201)
}
