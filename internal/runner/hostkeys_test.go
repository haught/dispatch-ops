package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"dispatch-ops/internal/runner/runnerapi"
)

// L9: the inventory and extra vars, which often hold secrets, are written
// to the memory-backed secrets volume, not the disk-backed work directory.
func TestPrepareWritesVarsAndInventoryToSecrets(t *testing.T) {
	cfg := Config{WorkDir: t.TempDir(), JobID: uuid.New()}
	inv := runnerapi.InventoryExport{}
	spec := &runnerapi.RunnerJobSpec{JobType: runnerapi.Script, Inventory: &inv, ExtraVars: map[string]any{"db_password": "x"}}
	ws, err := prepare(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"vars": ws.vars, "inventory": ws.inventory} {
		if !strings.HasPrefix(p, ws.secrets+string(filepath.Separator)) {
			t.Errorf("%s file %s is outside the secrets directory %s", name, p, ws.secrets)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s file: %v", name, err)
		}
	}
}

// L9: a machine credential's known_hosts turns Ansible's host key checking on.
func TestAnsibleKnownHosts(t *testing.T) {
	ws := testWorkspace(t)
	if _, err := ansibleCredentialArgs([]runnerapi.RunnerCredential{
		{Name: "machine", Kind: runnerapi.UsernamePassword, Inputs: map[string]string{"username": "u", "password": "p"}},
	}, ws); err != nil {
		t.Fatal(err)
	}
	if env := ws.hostKeyEnv(); env != nil {
		t.Errorf("without known_hosts, hostKeyEnv = %v, want none", env)
	}

	ws = testWorkspace(t)
	const kh = "web1.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	if _, err := ansibleCredentialArgs([]runnerapi.RunnerCredential{
		{Name: "machine", Kind: runnerapi.Ssh, Inputs: map[string]string{"username": "u", "known_hosts": kh}},
	}, ws); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ws.knownHosts)
	if err != nil || string(b) != kh+"\n" {
		t.Fatalf("known_hosts file = %q, %v", b, err)
	}
	if !strings.HasPrefix(ws.knownHosts, ws.secrets) {
		t.Errorf("known_hosts at %s, outside the secrets directory", ws.knownHosts)
	}
	env := ws.hostKeyEnv()
	if !slices.Contains(env, "ANSIBLE_HOST_KEY_CHECKING=True") {
		t.Errorf("hostKeyEnv = %v, want host key checking on", env)
	}
	if !slices.ContainsFunc(env, func(s string) bool {
		return strings.HasPrefix(s, "ANSIBLE_SSH_COMMON_ARGS=") && strings.Contains(s, "StrictHostKeyChecking=yes") &&
			strings.Contains(s, "UserKnownHostsFile="+shellQuote(ws.knownHosts))
	}) {
		t.Errorf("hostKeyEnv = %v, want ssh args using the known_hosts file", env)
	}
}

// L9: an SCM credential's known_hosts makes git's ssh require those keys.
func TestGitKnownHosts(t *testing.T) {
	sshCommand := func(cred *runnerapi.RunnerCredential) string {
		t.Helper()
		ws := testWorkspace(t)
		env, cleanup, err := gitEnv(Config{}, ws, &runnerapi.RunnerProject{Url: "git@git.example.com:a/b.git", Credential: cred})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GIT_SSH_COMMAND="); ok {
				return v
			}
		}
		t.Fatal("no GIT_SSH_COMMAND")
		return ""
	}
	if got := sshCommand(nil); !strings.Contains(got, "StrictHostKeyChecking=accept-new") {
		t.Errorf("without known_hosts: %s", got)
	}
	got := sshCommand(&runnerapi.RunnerCredential{Name: "scm", Kind: runnerapi.Scm, Inputs: map[string]string{
		"known_hosts": "git.example.com ssh-ed25519 AAAA",
	}})
	if !strings.Contains(got, "StrictHostKeyChecking=yes") || strings.Contains(got, "accept-new") {
		t.Errorf("with known_hosts: %s", got)
	}
}
