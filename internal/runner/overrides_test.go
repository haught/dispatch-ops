package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"dispatch-ops/internal/runner/runnerapi"
)

// fetchSpec picks up the launcher's extra vars when the server sends them
// (overrideVars), and does without when it doesn't.
func TestFetchSpecOverrideVars(t *testing.T) {
	body := `{"jobId": "` + uuid.NewString() + `", "jobType": "ansible", "command": [], "limit": "", "timeoutSeconds": 60,
		"inventory": null, "credentials": [], "project": null, "entrypoint": "site.yml", "arguments": [], "tags": "",
		"skipTags": "", "verbosity": 0, "extraVars": {"a": 1, "greeting": "hi"}`
	for _, tc := range []struct {
		extra string
		want  map[string]any
	}{
		{``, nil},
		{`, "overrideVars": {"greeting": "hi"}`, map[string]any{"greeting": "hi"}},
		// Numbers are kept exactly as sent, not rounded through a float64.
		{`, "overrideVars": {"big": 123456789012345678, "l": [1e15]}`, map[string]any{
			"big": json.Number("123456789012345678"), "l": []any{json.Number("1e15")},
		}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body + tc.extra + "}"))
		}))
		c, err := newClient(srv.URL, "tok", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		spec, over, err := fetchSpec(context.Background(), c, Config{JobID: uuid.New(), Version: "test"})
		srv.Close()
		if err != nil || spec == nil || spec.ExtraVars["greeting"] != "hi" || !reflect.DeepEqual(over, tc.want) {
			t.Errorf("%q: spec %v, overrides %v, err %v", tc.extra, spec, over, err)
		}
	}
}

func TestAnsibleVarArgs(t *testing.T) {
	ws := testWorkspace(t)
	ws.vars = ws.path("vars.json")
	spec := &runnerapi.RunnerJobSpec{ExtraVars: runnerapi.Variables{"tvar": "{{ 1 + 1 }}", "greeting": "hi"}}

	// Without overrides, all vars come from the one file, as before.
	args, err := ansibleVarArgs(spec, ws)
	if err != nil || !slices.Equal(args, []string{"-e", "@" + ws.vars}) {
		t.Fatalf("args = %q, %v", args, err)
	}

	ws.overrideVars = map[string]any{"greeting": "{{ x }}?", "list": []any{"a", map[string]any{"k": "v"}}}
	// The launcher's values could hold anything if the API missed it; the
	// runner checks again rather than rely on the unsafe marking alone.
	if _, err := ansibleVarArgs(spec, ws); err == nil {
		t.Error("Jinja in an override was accepted")
	}
	for _, bad := range []map[string]any{
		{"inventory_hostname": "evil.invalid"},
		{"ansible_ssh_common_args": "-o ProxyCommand=true"},
		{"x": "#jinja2:variable_start_string:'[['"},
		// The JSON loader would decrypt this with the template's vault
		// password, or mark it unsafe.
		{"x": map[string]any{"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256\n6162"}},
		{"__ansible_unsafe": "x"},
	} {
		ws.overrideVars = bad
		if _, err := ansibleVarArgs(spec, ws); err == nil {
			t.Errorf("override %v was accepted", bad)
		}
	}

	ws.overrideVars = map[string]any{"greeting": "hello", "list": []any{"a", map[string]any{"k": "v"}}}
	args, err = ansibleVarArgs(spec, ws)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, over := ws.secret("template-vars.json"), ws.secret("override-vars.json")
	// The overrides come last, so they still take precedence.
	if !slices.Equal(args, []string{"-e", "@" + tmpl, "-e", "@" + over}) {
		t.Fatalf("args = %q", args)
	}
	var got map[string]any
	b, _ := os.ReadFile(tmpl)
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, map[string]any{"tvar": "{{ 1 + 1 }}"}) {
		t.Errorf("template vars = %s (%v)", b, err)
	}
	b, _ = os.ReadFile(over)
	if want := `{"greeting":{"__ansible_unsafe":"hello"},"list":[{"__ansible_unsafe":"a"},{"k":{"__ansible_unsafe":"v"}}]}` + "\n"; string(b) != want {
		t.Errorf("overrides = %s, want %s", b, want)
	}
}

func TestUnsafeJSON(t *testing.T) {
	b, err := unsafeJSON(map[string]any{
		"s": "a\"b\\c\n<&>", "n": 3.0, "neg": -2.0, "f": 0.5, "big": 1e21, "small": 1e-7, "fifteen": 1e15,
		"t": true, "z": nil, "e": map[string]any{}, "l": []any{}, "i": 7, "i64": int64(-8),
		"num": json.Number("123456789012345678"), "huge": json.Number("123456789012345678901234567890"),
		"nexp": json.Number("1e15"), "nfrac": json.Number("3.0"), "nf": json.Number("0.5"), "nbig": json.Number("1e300"),
		"__ansible_unsafe_key": "keys aren't wrapped",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"__ansible_unsafe_key":{"__ansible_unsafe":"keys aren't wrapped"},"big":1e+21,"e":{},"f":0.5,` +
		`"fifteen":1000000000000000,"huge":1.2345678901234568e+29,"i":7,"i64":-8,"l":[],"n":3,` +
		`"nbig":1e+300,"neg":-2,"nexp":1000000000000000,"nf":0.5,"nfrac":3,"num":123456789012345678,` +
		`"s":{"__ansible_unsafe":"a\"b\\c\n\u003c\u0026\u003e"},"small":1e-7,"t":true,"z":null}` + "\n"
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	for _, bad := range []any{struct{}{}, math.NaN(), math.Inf(1), json.Number("x"), json.Number(strings.Repeat("9", 5000))} {
		if _, err := unsafeJSON(map[string]any{"x": []any{bad}}); err == nil {
			t.Errorf("%#v was written", bad)
		}
	}
}

// findAnsiblePlaybook returns the path of ansible-playbook, or skips the
// test if it isn't installed.
func findAnsiblePlaybook(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("ansible-playbook"); err == nil {
		return p
	}
	if _, err := os.Stat("/opt/ansible/bin/ansible-playbook"); err == nil {
		return "/opt/ansible/bin/ansible-playbook"
	}
	t.Skip("ansible-playbook is not installed")
	return ""
}

// TestOverridesNotTemplatedByAnsible runs the real ansible-playbook with
// the files ansibleVarArgs writes: the launcher's values, which the API
// checks would normally refuse, come out literally (R2-H1), with their
// types, while the template's own vars are still templated.
func TestOverridesNotTemplatedByAnsible(t *testing.T) {
	playbookCmd := findAnsiblePlaybook(t)
	ws := testWorkspace(t)
	canary := filepath.Join(ws.dir, "canary.txt")
	if err := os.WriteFile(canary, []byte("CANARY_SECRET_123"), 0o600); err != nil {
		t.Fatal(err)
	}
	header := "#jinja2:variable_start_string:'[[', variable_end_string:']]'\n[[ lookup('file', '" + canary + "') ]]"
	lookup := "{{ lookup('file', '" + canary + "') }}"
	overrides := map[string]any{
		"greeting": header,
		"nested":   map[string]any{"inner": []any{lookup, 1.0, true, nil}, "k": "{% if true %}EVAL{% endif %}"},
		"n":        3.0, "f": 0.5, "big": 1e21,
		"u": "é\x7f\u0085 😀 \"q\" \\",
	}
	ws.vars = ws.path("vars.json")
	spec := &runnerapi.RunnerJobSpec{ExtraVars: runnerapi.Variables{"tvar": "{{ 1 + 1 }}", "greeting": "template default"}}
	ws.overrideVars = overrides
	// Bypass the runner's own check, to see what Ansible does with the file.
	b, err := unsafeJSON(overrides)
	if err != nil {
		t.Fatal(err)
	}
	over := ws.secret("override-vars.json")
	tmpl := ws.secret("template-vars.json")
	if err := os.WriteFile(over, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(tmpl, map[string]any{"tvar": spec.ExtraVars["tvar"], "greeting": "template default"}, 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(ws.dir, "out.json")
	playbook := filepath.Join(ws.dir, "pb.yml")
	pb := `- hosts: localhost
  gather_facts: false
  tasks:
    - copy:
        dest: ` + out + `
        content: "{{ {'greeting': greeting, 'nested': nested, 'n': n, 'f': f, 'big': big, 'u': u, 'tvar': tvar, 'hello': 'Hello ' ~ greeting} | to_json }}"
`
	if err := os.WriteFile(playbook, []byte(pb), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(playbookCmd, "-i", "localhost,", "-c", "local", "-e", "@"+tmpl, "-e", "@"+over, playbook)
	cmd.Env = append(os.Environ(), "ANSIBLE_LOCAL_TEMP="+ws.path("tmp"), "ANSIBLE_REMOTE_TEMP="+ws.path("rtmp"), "ANSIBLE_NOCOLOR=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook: %v\n%s", err, output)
	}
	got, _ := os.ReadFile(out)
	if strings.Contains(string(got), "CANARY") || strings.Contains(string(output), "CANARY") {
		t.Fatalf("an override was templated:\n%s\n%s", got, output)
	}
	var vars map[string]any
	if err := json.Unmarshal(got, &vars); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	want := map[string]any{"tvar": 2.0, "hello": "Hello " + header}
	for k, v := range overrides {
		want[k] = v
	}
	for k := range want {
		if !reflect.DeepEqual(vars[k], want[k]) {
			t.Errorf("Ansible saw %s = %#v, want %#v", k, vars[k], want[k])
		}
	}
}

// TestOverrideTypesKeptByAnsible runs the real ansible-playbook with an
// override file unsafeJSON writes from values decoded as fetchSpec decodes
// them: each reaches the play with its type and value, strings that YAML
// would resolve to something else included, and templates come out
// literally.
func TestOverrideTypesKeptByAnsible(t *testing.T) {
	playbookCmd := findAnsiblePlaybook(t)
	ws := testWorkspace(t)
	const sent = `{"greeting":"", "ver":"1.10", "zip":"01234", "msg":"no", "t":"12:30", "x":"0x1F", "m":"<<", "e":"=",
		"d":"2024-01-01", "big":123456789012345678, "n":1e15, "list":[1,"a",{"k":"8080"}], "tpl":"{{ 7*7 }}",
		"hdr":"#jinja2:variable_start_string:'[[', variable_end_string:']]'\n[[ 7*7 ]]"}`
	decode := func(s string) map[string]any {
		t.Helper()
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
		return m
	}
	overrides := decode(sent)
	b, err := unsafeJSON(overrides)
	if err != nil {
		t.Fatal(err)
	}
	over := ws.secret("override-vars.json")
	if err := os.WriteFile(over, b, 0o600); err != nil {
		t.Fatal(err)
	}

	names := slices.Sorted(maps.Keys(overrides))
	var vals, types []string
	for _, k := range names {
		vals = append(vals, fmt.Sprintf("'%s': %s", k, k))
		types = append(types, fmt.Sprintf("'%s': %s | type_debug", k, k))
	}
	types = append(types, "'list0': list[0] | type_debug", "'list1': list[1] | type_debug", "'listk': list[2].k | type_debug")
	out := filepath.Join(ws.dir, "out.json")
	playbook := filepath.Join(ws.dir, "pb.yml")
	pb := `- hosts: localhost
  gather_facts: false
  tasks:
    - copy:
        dest: ` + out + `
        content: "{{ {'vals': {` + strings.Join(vals, ", ") + `}, 'types': {` + strings.Join(types, ", ") + `}} | to_json }}"
`
	if err := os.WriteFile(playbook, []byte(pb), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(playbookCmd, "-i", "localhost,", "-c", "local", "-e", "@"+over, playbook)
	cmd.Env = append(os.Environ(), "ANSIBLE_LOCAL_TEMP="+ws.path("tmp"), "ANSIBLE_REMOTE_TEMP="+ws.path("rtmp"), "ANSIBLE_NOCOLOR=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook: %v\n%s", err, output)
	}
	got, _ := os.ReadFile(out)
	var res struct {
		Vals  map[string]any    `json:"vals"`
		Types map[string]string `json:"types"`
	}
	dec := json.NewDecoder(strings.NewReader(string(got)))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	want := overrides
	want["n"] = json.Number("1000000000000000")
	for _, k := range names {
		if !reflect.DeepEqual(res.Vals[k], want[k]) {
			t.Errorf("Ansible saw %s = %#v, want %#v", k, res.Vals[k], want[k])
		}
	}
	wantTypes := map[string]string{"big": "int", "n": "int", "list": "list", "list0": "int", "list1": "str", "listk": "str"}
	for _, k := range names {
		if _, ok := wantTypes[k]; !ok {
			wantTypes[k] = "str"
		}
	}
	for k, typ := range wantTypes {
		// Unsafe strings are a str subclass in some versions.
		if got := res.Types[k]; got != typ && !(typ == "str" && strings.Contains(strings.ToLower(got), "unsafe")) {
			t.Errorf("%s | type_debug = %q, want %q", k, got, typ)
		}
	}
}
