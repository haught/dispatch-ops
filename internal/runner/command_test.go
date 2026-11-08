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
	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ":     "bob\n",
		"Password for 'https://bob@github.com': ": "s3cret\n",
	} {
		var out bytes.Buffer
		if code := Askpass(dir, []string{prompt}, &out); code != 0 || out.String() != want {
			t.Errorf("%q: code %d, output %q", prompt, code, out.String())
		}
	}
	var out bytes.Buffer
	if code := Askpass(dir, []string{"Are you sure (yes/no)?"}, &out); code == 0 {
		t.Error("answered an unknown prompt")
	}
}
