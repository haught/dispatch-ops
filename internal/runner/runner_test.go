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

func TestPrepareSetsGenericCredentialEnv(t *testing.T) {
	cfg := Config{WorkDir: t.TempDir(), JobID: uuid.New()}
	spec := &runnerapi.RunnerJobSpec{
		JobType: runnerapi.Script,
		Credentials: []runnerapi.RunnerCredential{
			{Id: uuid.New(), Name: "akips", Kind: runnerapi.Generic, Inputs: map[string]string{
				"AKIPS_HOST": "https://akips.example.com", "AKIPS_PASSWORD": "s3cret", "PATH": "/evil",
			}},
			{Id: uuid.New(), Name: "machine", Kind: runnerapi.UsernamePassword, Inputs: map[string]string{
				"username": "deploy", "password": "pw",
			}},
		},
	}
	ws, err := prepare(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) []string {
		var vals []string
		for _, kv := range ws.env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				vals = append(vals, v)
			}
		}
		return vals
	}
	if got := lookup("AKIPS_HOST"); !slices.Equal(got, []string{"https://akips.example.com"}) {
		t.Errorf("AKIPS_HOST = %v", got)
	}
	if got := lookup("AKIPS_PASSWORD"); !slices.Equal(got, []string{"s3cret"}) {
		t.Errorf("AKIPS_PASSWORD = %v", got)
	}
	if slices.Contains(lookup("PATH"), "/evil") {
		t.Error("a reserved name was set from a credential")
	}
	if slices.Contains(lookup("username"), "deploy") || len(lookup("password")) > 0 {
		t.Error("non-generic credential inputs were set as environment variables")
	}
	notices := strings.Join(ws.notices, "\n")
	if want := `"akips": PATH is a reserved name`; !strings.Contains(notices, want) {
		t.Errorf("notices %q lack %q", notices, want)
	}
}

// Two credentials that set the same variable would let one decide where the
// other's secrets go (VAULT_ADDR beside VAULT_TOKEN), so the job is refused
// rather than one silently winning.
func TestPrepareRefusesConflictingCredentialEnv(t *testing.T) {
	token := runnerapi.RunnerCredential{Id: uuid.New(), Name: "vault", Kind: runnerapi.Generic, Inputs: map[string]string{
		"VAULT_ADDR": "https://vault.example.com", "VAULT_TOKEN": "s3cret",
	}}
	for _, other := range []map[string]string{
		{"VAULT_ADDR": "https://evil.example"},
		{"vault_addr": "https://evil.example"}, // some tools read either case
	} {
		dir := t.TempDir()
		spec := &runnerapi.RunnerJobSpec{
			JobType: runnerapi.Script,
			Credentials: []runnerapi.RunnerCredential{
				token, {Id: uuid.New(), Name: "mine", Kind: runnerapi.Generic, Inputs: other},
			},
		}
		_, err := prepare(Config{WorkDir: dir, JobID: uuid.New()}, spec)
		if err == nil || !strings.Contains(err.Error(), `credentials "vault" and "mine" both set the environment variable`) {
			t.Errorf("%v: err = %v, want a refusal naming both credentials", other, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "secrets")); len(entries) > 0 {
			t.Errorf("%v: a refused job wrote secrets: %v", other, entries)
		}
	}

	// A credential listed twice, or other kinds' inputs of the same name,
	// are no conflict.
	spec := &runnerapi.RunnerJobSpec{
		JobType: runnerapi.Script,
		Credentials: []runnerapi.RunnerCredential{
			token, token,
			{Id: uuid.New(), Name: "api", Kind: runnerapi.ApiToken, Inputs: map[string]string{"url": "https://x", "token": "t"}},
			{Id: uuid.New(), Name: "api2", Kind: runnerapi.ApiToken, Inputs: map[string]string{"url": "https://y", "token": "u"}},
		},
	}
	if _, err := prepare(Config{WorkDir: t.TempDir(), JobID: uuid.New()}, spec); err != nil {
		t.Error(err)
	}
}
