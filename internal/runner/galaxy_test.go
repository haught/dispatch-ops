package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"dispatch-ops/internal/runner/runnerapi"
)

func writeProjectFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRequirementsFiles(t *testing.T) {
	dir := t.TempDir()
	if got := requirementsFiles(dir); len(got) != 0 {
		t.Fatalf("empty project: %v", got)
	}
	writeProjectFile(t, dir, "collections/requirements.yaml", "collections: []\n")
	writeProjectFile(t, dir, "roles/requirements.yml", "roles: []\n")
	writeProjectFile(t, dir, "requirements.yml", "collections: []\n")
	writeProjectFile(t, dir, "requirements.yaml", "collections: []\n") // .yml wins
	if err := os.MkdirAll(filepath.Join(dir, "other/requirements.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []galaxyStep{
		{file: "roles/requirements.yml", kind: "role"},
		{file: "collections/requirements.yaml", kind: "collection"},
		{file: "requirements.yml", kind: ""},
	}
	if got := requirementsFiles(dir); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// fakeGalaxy puts an ansible-galaxy on PATH that appends its arguments and
// some of its environment to a log, and exits with code.
func fakeGalaxy(t *testing.T, code int) (logFile string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(bin, "calls.log")
	script := `#!/bin/sh
echo "args: $*" >> "` + logFile + `"
echo "cwd: $(pwd)" >> "` + logFile + `"
echo "collections: $ANSIBLE_COLLECTIONS_PATH" >> "` + logFile + `"
echo "askpass: $GIT_ASKPASS" >> "` + logFile + `"
echo "installing from $3"
exit ` + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ansible-galaxy"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return logFile
}

func TestInstallRequirements(t *testing.T) {
	logFile := fakeGalaxy(t, 0)
	ws := testWorkspace(t)
	ws.projectDir = filepath.Join(ws.dir, "project")
	writeProjectFile(t, ws.projectDir, "collections/requirements.yml", "collections: []\n")
	writeProjectFile(t, ws.projectDir, "requirements.yml", "roles: []\n")
	project := &runnerapi.RunnerProject{Credential: &runnerapi.RunnerCredential{
		Name: "scm", Inputs: map[string]string{"username": "u", "password": "p"},
	}}
	var said []string
	var out bytes.Buffer
	cfg := Config{Askpass: "/bin/dispatch-runner"}
	err := installRequirements(context.Background(), cfg, ws, project, 2, func(s string) { said = append(said, s) }, &out)
	if err != nil {
		t.Fatal(err)
	}

	collections := ws.path("ansible", "collections")
	log, _ := os.ReadFile(logFile)
	for _, want := range []string{
		"args: collection install -r collections/requirements.yml -p " + collections + " -vv\n",
		"args: install -r requirements.yml -vv\n",
		"cwd: " + ws.projectDir + "\n",
		"collections: " + collections + ":" + defaultCollectionsPath + "\n",
		"askpass: /bin/dispatch-runner\n",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("calls log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(out.String(), "installing from requirements.yml") {
		t.Errorf("output = %q", out.String())
	}
	if len(said) != 2 || !strings.Contains(said[0], "collections/requirements.yml") {
		t.Errorf("messages = %q", said)
	}
	// The SCM credential's files are gone afterwards.
	if _, err := os.Stat(ws.secret("scm")); !os.IsNotExist(err) {
		t.Errorf("scm secrets left behind: %v", err)
	}

	// ansible-playbook gets the search paths.
	writeProjectFile(t, ws.projectDir, "site.yml", "- hosts: all\n")
	ws.vars = filepath.Join(ws.dir, "vars.json")
	c, err := ansibleCommand(&runnerapi.RunnerJobSpec{Entrypoint: "site.yml"}, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.env, "ANSIBLE_COLLECTIONS_PATH="+collections+":"+defaultCollectionsPath) ||
		!slices.Contains(c.env, "ANSIBLE_ROLES_PATH="+ws.path("ansible", "roles")+":"+defaultRolesPath) {
		t.Errorf("playbook env lacks the search paths: %q", c.env)
	}
}

func TestInstallRequirementsNone(t *testing.T) {
	logFile := fakeGalaxy(t, 0)
	ws := testWorkspace(t)
	ws.projectDir = t.TempDir()
	if err := installRequirements(context.Background(), Config{}, ws, nil, 0, func(string) {}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Error("ansible-galaxy ran without requirements files")
	}
	if ws.ansibleEnv != nil {
		t.Errorf("ansibleEnv = %q", ws.ansibleEnv)
	}
}

func TestInstallRequirementsFails(t *testing.T) {
	fakeGalaxy(t, 1)
	ws := testWorkspace(t)
	ws.projectDir = t.TempDir()
	writeProjectFile(t, ws.projectDir, "roles/requirements.yml", "roles: []\n")
	err := installRequirements(context.Background(), Config{}, ws, nil, 0, func(string) {}, &bytes.Buffer{})
	if err == nil || err.Error() != "roles/requirements.yml: ansible-galaxy exited with code 1" {
		t.Errorf("err = %v", err)
	}
	if ws.ansibleEnv != nil {
		t.Errorf("ansibleEnv = %q", ws.ansibleEnv)
	}
}
