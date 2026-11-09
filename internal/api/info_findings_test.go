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
