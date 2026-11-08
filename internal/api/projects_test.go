package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjects(t *testing.T) {
	c := signedIn(t)

	scm := c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("scm"), "kind": "scm", "inputs": map[string]string{"username": "u", "password": "p"},
	}, 201)["id"].(string)
	gen := c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("gen"), "kind": "generic", "inputs": map[string]string{"X": "y"},
	}, 201)["id"].(string)

	for _, url := range []string{"https://github.com/acme/ops.git", "git@github.com:acme/ops.git", "ssh://git@host:22/x", "file:///srv/repo"} {
		p := c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": url}, 201)
		if p["scmRef"] != "main" || p["credentialId"] != nil {
			t.Errorf("project for %s = %v", url, p)
		}
	}
	for _, url := range []string{"ext::sh -c touch% /tmp/x", "ext::sh", "ftp://host/x", "/srv/repo", "-uploadpack=x", "relative/path"} {
		c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": url}, 400)
	}
	c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": "https://x/y", "scmRef": "--upload-pack=x"}, 400)
	// Only scm credentials clone repositories.
	c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": "https://x/y", "credentialId": gen}, 400)

	p := c.mustDo("POST", "/api/v1/projects", map[string]any{
		"name": unique("p"), "scmUrl": "https://x/y", "scmRef": "v1.2", "credentialId": scm,
	}, 201)
	id := p["id"].(string)
	p = c.mustDo("PATCH", "/api/v1/projects/"+id, map[string]any{"scmRef": "release"}, 200)
	if p["credentialId"] != scm || p["scmRef"] != "release" {
		t.Errorf("PATCH scmRef = %v", p)
	}
	p = c.mustDo("PATCH", "/api/v1/projects/"+id, map[string]any{"credentialId": nil}, 200)
	if p["credentialId"] != nil {
		t.Errorf("PATCH credentialId null = %v", p)
	}
	list := c.mustDo("GET", "/api/v1/projects?search="+p["name"].(string), nil, 200)
	if list["total"].(float64) != 1 {
		t.Errorf("search = %v", list)
	}
	c.mustDo("DELETE", "/api/v1/projects/"+id, nil, 204)
	c.mustDo("GET", "/api/v1/projects/"+id, nil, 404)
}

func TestProjectTemplates(t *testing.T) {
	c := signedIn(t)
	ee := c.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "x"}, 201)["id"].(string)
	proj := c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": "https://x/y"}, 201)["id"].(string)
	cred := func(kind string, inputs map[string]string) string {
		return c.mustDo("POST", "/api/v1/credentials", map[string]any{"name": unique(kind), "kind": kind, "inputs": inputs}, 201)["id"].(string)
	}
	ssh1 := cred("ssh", map[string]string{"private_key": "k"})
	ssh2 := cred("username_password", map[string]string{"username": "u", "password": "p"})
	vaultA := cred("vault", map[string]string{"vault_id": "prod", "password": "a"})
	vaultB := cred("vault", map[string]string{"vault_id": "prod", "password": "b"})
	vaultC := cred("vault", map[string]string{"password": "c"})
	c.mustDo("POST", "/api/v1/credentials", map[string]any{"name": unique("v"), "kind": "vault", "inputs": map[string]string{"vault_id": "a@b", "password": "x"}}, 400)

	tmpl := func(fields map[string]any) map[string]any {
		body := map[string]any{"name": unique("jt"), "executionEnvironmentId": ee}
		for k, v := range fields {
			body[k] = v
		}
		return body
	}
	bad := []map[string]any{
		{"jobType": "ansible"},                    // no project
		{"jobType": "ansible", "projectId": proj}, // no entrypoint
		{"jobType": "script", "projectId": proj, "entrypoint": "../x.sh"},
		{"jobType": "script", "projectId": proj, "entrypoint": "/etc/passwd"},
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "command": []string{"x"}},
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "arguments": []string{"x"}},
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "allowOverrides": []string{"command"}},
		{"jobType": "script", "projectId": proj, "entrypoint": "x.sh", "tags": "a"},
		{"jobType": "script", "projectId": proj, "entrypoint": "x.sh", "allowOverrides": []string{"tags"}},
		{"jobType": "container", "command": []string{"x"}, "projectId": proj},
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "verbosity": 6},
		// One machine credential, one vault password per vault ID.
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "credentialIds": []string{ssh1, ssh2}},
		{"jobType": "ansible", "projectId": proj, "entrypoint": "site.yml", "credentialIds": []string{vaultA, vaultB}},
	}
	for _, b := range bad {
		resp, body := c.do("POST", "/api/v1/job-templates", tmpl(b))
		if resp.StatusCode != 400 {
			t.Errorf("create %v = %d %v, want 400", b, resp.StatusCode, body)
		}
	}

	jt := c.mustDo("POST", "/api/v1/job-templates", tmpl(map[string]any{
		"jobType": "ansible", "projectId": proj, "entrypoint": "playbooks/site.yml", "tags": "deploy",
		"verbosity": 2, "credentialIds": []string{ssh1, vaultA, vaultC}, "allowOverrides": []string{"tags", "limit"},
	}), 201)
	id := jt["id"].(string)
	if jt["projectId"] != proj || jt["entrypoint"] != "playbooks/site.yml" || jt["verbosity"].(float64) != 2 {
		t.Errorf("created = %v", jt)
	}
	// Updates are validated as a whole.
	c.mustDo("PATCH", "/api/v1/job-templates/"+id, map[string]any{"entrypoint": "../up.yml"}, 400)
	c.mustDo("PATCH", "/api/v1/job-templates/"+id, map[string]any{"credentialIds": []string{ssh1, ssh2}}, 400)
	c.mustDo("PATCH", "/api/v1/job-templates/"+id, map[string]any{"skipTags": "slow"}, 200)

	// The project is snapshotted into the job.
	c.mustDo("DELETE", "/api/v1/projects/"+proj, nil, 409)
	c.mustDo("POST", "/api/v1/job-templates/"+id+"/launch", map[string]any{"skipTags": "x"}, 400)
	job := c.mustDo("POST", "/api/v1/job-templates/"+id+"/launch", map[string]any{"tags": "web"}, 201)
	cfg := job["config"].(map[string]any)
	p := cfg["project"].(map[string]any)
	if p["id"] != proj || p["scmUrl"] != "https://x/y" || p["scmRef"] != "main" || cfg["tags"] != "web" ||
		cfg["skipTags"] != "slow" || cfg["entrypoint"] != "playbooks/site.yml" || job["scmRevision"] != "" {
		t.Errorf("job = %s", jsonOf(job))
	}
	c.mustDo("PATCH", "/api/v1/projects/"+proj, map[string]any{"scmRef": "other"}, 200)
	job = c.mustDo("GET", "/api/v1/jobs/"+job["id"].(string), nil, 200)
	if job["config"].(map[string]any)["project"].(map[string]any)["scmRef"] != "main" {
		t.Errorf("job's project changed with the project: %v", job["config"])
	}

	script := c.mustDo("POST", "/api/v1/job-templates", tmpl(map[string]any{
		"jobType": "script", "projectId": proj, "entrypoint": "bin/deploy.sh", "arguments": []string{"--fast", "x y"},
	}), 201)
	if strs(script["arguments"])[1] != "x y" {
		t.Errorf("script template = %v", script)
	}
}

// gitRepo makes a git repository with the given files and returns its
// file:// URL and the commit ID.
func gitRepo(t *testing.T, files map[string]string) (url, commit string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "init.defaultBranch=main", "-c", "commit.gpgSign=false"}, args...)...)
		cmd.Dir = dir
		// Not the developer's own git settings.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "test")
	return "file://" + dir, git("rev-parse", "HEAD")
}
