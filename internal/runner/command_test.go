package runner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"dispatch-ops/internal/runner/runnerapi"
)

func testWorkspace(t *testing.T) *workspace {
	t.Helper()
	dir := t.TempDir()
	ws := &workspace{dir: dir, secrets: filepath.Join(dir, "secrets")}
	for _, d := range []string{ws.path(), ws.secret()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func encryptedKey(t *testing.T, passphrase string) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), priv
}

func TestAnsibleCredentialArgs(t *testing.T) {
	ws := testWorkspace(t)
	key, priv := encryptedKey(t, "pp")
	args, err := ansibleCredentialArgs([]runnerapi.RunnerCredential{
		{Name: "machine", Kind: runnerapi.Ssh, Inputs: map[string]string{
			"username": "deploy", "private_key": key, "passphrase": "pp", "become_password": "sudo!",
		}},
		{Name: "v1", Kind: runnerapi.Vault, Inputs: map[string]string{"vault_id": "prod", "password": "p1"}},
		{Name: "v2", Kind: runnerapi.Vault, Inputs: map[string]string{"password": "p2"}},
		{Name: "api", Kind: runnerapi.ApiToken, Inputs: map[string]string{"token": "t"}},
	}, ws)
	if err != nil {
		t.Fatal(err)
	}
	opt := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("no %s in %v", name, args)
		}
		return args[i+1]
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p, ws.secrets) {
			t.Errorf("%s is not in the secrets directory", p)
		}
		return string(b)
	}
	if opt("-u") != "deploy" || read(opt("--become-password-file")) != "sudo!" {
		t.Errorf("args = %v", args)
	}
	// The key is written without its passphrase.
	parsed, err := ssh.ParseRawPrivateKey([]byte(read(opt("--private-key"))))
	if err != nil {
		t.Fatalf("written key: %v", err)
	}
	if got := *parsed.(*ed25519.PrivateKey); !got.Equal(priv) {
		t.Error("written key is a different key")
	}
	id, file, _ := strings.Cut(opt("--vault-id"), "@")
	if id != "prod" || read(file) != "p1" || read(opt("--vault-password-file")) != "p2" {
		t.Errorf("vault args = %v", args)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm()&0o111 != 0 {
		t.Error("vault password file is executable; Ansible would run it")
	}

	// A wrong passphrase, and two machine credentials, are errors.
	_, err = ansibleCredentialArgs([]runnerapi.RunnerCredential{
		{Name: "m", Kind: runnerapi.Ssh, Inputs: map[string]string{"private_key": key, "passphrase": "wrong"}},
	}, ws)
	if err == nil || !strings.Contains(err.Error(), "passphrase is wrong") {
		t.Errorf("wrong passphrase: %v", err)
	}
	_, err = ansibleCredentialArgs([]runnerapi.RunnerCredential{
		{Name: "a", Kind: runnerapi.Ssh, Inputs: map[string]string{"private_key": "k"}},
		{Name: "b", Kind: runnerapi.UsernamePassword, Inputs: map[string]string{"username": "u", "password": "p"}},
	}, ws)
	if err == nil {
		t.Error("two machine credentials were accepted")
	}
}

func TestWritePrivateKeyWithoutPassphrase(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	key := strings.TrimSuffix(string(pem.EncodeToMemory(block)), "\n")
	p := filepath.Join(t.TempDir(), "k")
	// A passphrase given for a key that has none is ignored.
	if err := writePrivateKey(p, key, "unneeded"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != key+"\n" {
		t.Error("key was changed")
	}
}

func TestStaticInventory(t *testing.T) {
	var export map[string]any
	_ = json.Unmarshal([]byte(`{
		"all": {"children": ["ungrouped", "servers"], "vars": {"env": "prod"}},
		"ungrouped": {"hosts": ["lone"]},
		"servers": {"children": ["web"], "vars": {"tier": 1}},
		"web": {"hosts": ["web1"]},
		"_meta": {"hostvars": {"lone": {}, "web1": {"port": 80}}}
	}`), &export)
	got, _ := json.Marshal(staticInventory(export))
	want := `{"all":{"children":{"servers":{"children":{"web":null},"vars":{"tier":1}},"web":{"hosts":{"web1":null}}},` +
		`"hosts":{"lone":{},"web1":{"port":80}},"vars":{"env":"prod"}}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestParseEventLine(t *testing.T) {
	const m = "\x1edispatch-event:abc:"
	typ, host, data, ok := parseEventLine(m+`{"type":"host_ok","host":"web1","data":{"changed":true}}`, m)
	if !ok || typ != "host_ok" || host != "web1" || data["changed"] != true {
		t.Errorf("got %q %q %v %v", typ, host, data, ok)
	}
	for _, line := range []string{
		`{"type":"host_ok"}`, // no marker
		"\x1edispatch-event:other:" + `{"type":"host_ok"}`, // another job's marker
		m + `{"type":"Bad Type"}`,
		m + `not json`,
	} {
		if _, _, _, ok := parseEventLine(line, m); ok {
			t.Errorf("%q was parsed as an event", line)
		}
	}
	if _, _, _, ok := parseEventLine(`{"type":"host_ok"}`, ""); ok {
		t.Error("parsed an event without a marker")
	}
}

func TestAskpass(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "username"), []byte("bob"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "origin"), []byte("https://github.com:443"), 0o600)
	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ":         "bob\n",
		"Password for 'https://bob@github.com': ":     "s3cret\n",
		"Password for 'https://bob@GitHub.com:443': ": "s3cret\n",
	} {
		var out bytes.Buffer
		if code := Askpass(dir, []string{prompt}, &out); code != 0 || out.String() != want {
			t.Errorf("%q: code %d, output %q", prompt, code, out.String())
		}
	}
	// Only the project's own server gets the credential: not a submodule or
	// galaxy requirement on another host, port or scheme.
	for _, prompt := range []string{
		"Are you sure (yes/no)?",
		"Username for 'https://attacker.example': ",
		"Password for 'https://bob@attacker.example': ",
		"Password for 'https://bob@github.com.attacker.example': ",
		"Password for 'https://bob@github.com:8443': ",
		"Password for 'http://bob@github.com': ",
		"Password for https://github.com: ",
		"Password for '': ",
	} {
		var out bytes.Buffer
		if code := Askpass(dir, []string{prompt}, &out); code == 0 || out.Len() > 0 {
			t.Errorf("%q: answered %q", prompt, out.String())
		}
	}
	// Without a known origin, nothing is answered.
	_ = os.Remove(filepath.Join(dir, "origin"))
	var out bytes.Buffer
	if code := Askpass(dir, []string{"Password for 'https://bob@github.com': "}, &out); code == 0 {
		t.Error("answered without an origin")
	}
}

func TestGitEnvOrigin(t *testing.T) {
	ws := testWorkspace(t)
	cred := &runnerapi.RunnerCredential{Name: "scm", Inputs: map[string]string{"username": "u", "password": "p"}}
	for url, want := range map[string]string{
		"https://Git.Example.com/org/repo.git":   "https://git.example.com:443",
		"http://git.example.com:8080/repo.git":   "http://git.example.com:8080",
		"https://user:tok@git.example.com/r.git": "https://git.example.com:443",
		"git@git.example.com:org/repo.git":       "",
	} {
		_, cleanup, err := gitEnv(Config{Askpass: "/bin/dispatch-runner"}, ws, &runnerapi.RunnerProject{Url: url, Credential: cred})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(ws.secret("scm"), "origin"))
		if string(got) != want {
			t.Errorf("%s: origin %q, want %q", url, got, want)
		}
		cleanup()
	}
}

func TestInventorySyncCommand(t *testing.T) {
	ws := testWorkspace(t)
	ws.projectDir = filepath.Join(ws.dir, "project")
	if err := os.MkdirAll(filepath.Join(ws.projectDir, "inventory"), 0o700); err != nil {
		t.Fatal(err)
	}
	ws.env = []string{"AKIPS_HOST=https://akips.example.com"}
	spec := &runnerapi.RunnerJobSpec{
		JobType: runnerapi.InventorySync, Entrypoint: "inventory", Verbosity: 2,
		Credentials: []runnerapi.RunnerCredential{
			{Name: "v", Kind: runnerapi.Vault, Inputs: map[string]string{"password": "pw"}},
			{Name: "g", Kind: runnerapi.Generic, Inputs: map[string]string{"AKIPS_HOST": "x"}},
		},
	}
	c, err := buildCommand(spec, ws)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ansible-inventory", "-i", filepath.Join(ws.projectDir, "inventory"), "--list", "--export",
		"--output", inventoryResultFile(ws), "-vv", "--vault-password-file"}
	if !slices.Equal(c.argv[:len(want)], want) || len(c.argv) != len(want)+1 {
		t.Errorf("argv = %q", c.argv)
	}
	if c.dir != ws.projectDir || c.eventMarker != "" {
		t.Errorf("dir = %q, marker = %q", c.dir, c.eventMarker)
	}
	for _, kv := range []string{"AKIPS_HOST=https://akips.example.com", "ANSIBLE_INVENTORY_UNPARSED_FAILED=True", "ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=True"} {
		if !slices.Contains(c.env, kv) {
			t.Errorf("env lacks %s", kv)
		}
	}

	// The source must be in the project.
	spec.Entrypoint = "../etc"
	if _, err := buildCommand(spec, ws); err == nil {
		t.Error("a source outside the project was accepted")
	}
}

func TestAnsibleCommandLimit(t *testing.T) {
	ws := testWorkspace(t)
	ws.projectDir = filepath.Join(ws.dir, "project")
	ws.vars = ws.path("vars.json")
	writeProjectFile(t, ws.projectDir, "site.yml", "- hosts: all\n")
	spec := &runnerapi.RunnerJobSpec{JobType: runnerapi.Ansible, Entrypoint: "site.yml", Limit: "web,db"}
	c, err := buildCommand(spec, ws)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(c.argv, "--limit"); i < 0 || c.argv[i+1] != "web,db" {
		t.Errorf("argv = %q", c.argv)
	}
	if !slices.Contains(c.env, "ANSIBLE_HOST_PATTERN_MISMATCH=ignore") {
		t.Errorf("env lacks ANSIBLE_HOST_PATTERN_MISMATCH=ignore")
	}

	// A limit naming a file would print it into the output: refused even
	// if the API let it through.
	for _, limit := range []string{"@/runner/secrets/credentials.json", "web,@/etc/passwd", "web:@/etc/passwd"} {
		spec.Limit = limit
		if _, err := buildCommand(spec, ws); err == nil {
			t.Errorf("limit %q was accepted", limit)
		}
	}
}

// A repository's symbolic links can't take the entrypoint outside the
// project, to the job's secrets for one.
func TestProjectPathSymlinks(t *testing.T) {
	ws := testWorkspace(t)
	ws.projectDir = filepath.Join(ws.dir, "project")
	writeProjectFile(t, ws.projectDir, "site.yml", "- hosts: all\n")
	writeProjectFile(t, ws.projectDir, "inv/hosts.yml", "all: {}\n")
	outside := filepath.Join(ws.secrets, "credentials.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"evil.yml":   outside,
		"rel.yml":    "../secrets/credentials.json",
		"secretsdir": ws.secrets,
		"inside.yml": "site.yml",
		"invlink":    "inv",
	} {
		if err := os.Symlink(target, filepath.Join(ws.projectDir, link)); err != nil {
			t.Fatal(err)
		}
	}
	spec := &runnerapi.RunnerJobSpec{JobType: runnerapi.Ansible}
	for _, e := range []string{"evil.yml", "rel.yml", "secretsdir/credentials.json"} {
		spec.Entrypoint = e
		if _, err := projectFile(spec, ws, "playbook"); err == nil || !strings.Contains(err.Error(), "outside the project") {
			t.Errorf("%s: err = %v, want refused", e, err)
		}
	}
	spec.Entrypoint = "secretsdir"
	if _, err := projectPath(spec, ws, "inventory source", true); err == nil {
		t.Error("a directory link to outside the project was accepted")
	}
	for e, dirOK := range map[string]bool{"inside.yml": false, "site.yml": false, "invlink": true, "invlink/hosts.yml": false} {
		spec.Entrypoint = e
		if p, err := projectPath(spec, ws, "entrypoint", dirOK); err != nil || p != filepath.Join(ws.projectDir, e) {
			t.Errorf("%s: %q, %v", e, p, err)
		}
	}

	// The project directory may itself be reached through a link.
	alias := filepath.Join(ws.dir, "alias")
	if err := os.Symlink(ws.projectDir, alias); err != nil {
		t.Fatal(err)
	}
	ws.projectDir = alias
	spec.Entrypoint = "inside.yml"
	if _, err := projectFile(spec, ws, "playbook"); err != nil {
		t.Errorf("project behind a link: %v", err)
	}
	spec.Entrypoint = "evil.yml"
	if _, err := projectFile(spec, ws, "playbook"); err == nil {
		t.Error("project behind a link: a link outside was accepted")
	}
}
