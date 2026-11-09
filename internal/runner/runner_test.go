package runner

import (
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
			{Id: uuid.New(), Name: "override", Kind: runnerapi.Generic, Inputs: map[string]string{
				"AKIPS_HOST": "https://other.example.com",
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
	if got := lookup("AKIPS_HOST"); !slices.Equal(got, []string{"https://other.example.com"}) {
		t.Errorf("AKIPS_HOST = %v, want the later credential's value once", got)
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
	for _, want := range []string{`"akips": PATH is a reserved name`, `"override" sets AKIPS_HOST, overriding credential "akips"`} {
		if !strings.Contains(notices, want) {
			t.Errorf("notices %q lack %q", notices, want)
		}
	}
}
