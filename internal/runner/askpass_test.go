package runner

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dispatch-ops/internal/runner/runnerapi"
)

func askpassDir(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for f, v := range map[string]string{"username": "bot", "password": "SECRET-PAT", "origin": origin} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// R2-M2 and the askpass hardening: look-alike hosts, and prompt URLs that
// Go could read differently from git, get no answer.
func TestAskpassRefusesLookalikesAndOddURLs(t *testing.T) {
	dir := askpassDir(t, "https://github.com:443")
	for _, prompt := range []string{
		"Password for 'https://bot@g%C4%B0thub.com': ",
		"Password for 'https://bot@gİthub.com': ",
		"Username for 'https://gİthub.com': ",
		"Password for 'https://bot@g%25C4%25B0thub.com': ",
		"Password for 'https://bot@ｇithub.com': ",
		// More than one @: which one ends the user name?
		"Password for 'https://a@b@github.com': ",
		"Password for 'https://github.com@evil.example': ",
		"Password for 'https://evil.example@a@github.com': ",
		// The sanitized form of http://victim%2F@evil/ is for host
		// evil.example, not the project's.
		"Password for 'https://github.com%2F@evil.example/': ",
		// git without credential.sanitizePrompt shows
		// http://victim%2F@evil/ as http://victim/@evil.
		"Password for 'https://github.com/@evil.example': ",
		"Password for 'https://github.com?@evil.example': ",
		"Password for 'https://github.com#@evil.example': ",
	} {
		var out bytes.Buffer
		if code := Askpass(dir, []string{prompt}, &out); code == 0 || out.Len() > 0 {
			t.Errorf("%q: answered %q", prompt, out.String())
		}
	}
	for _, prompt := range []string{
		"Password for 'https://bot@github.com': ",
		"Password for 'https://github.com/': ",
		// git encodes an @ in the user name (an email address); encoded
		// characters can't move the host.
		"Password for 'https://me%40corp.com@github.com': ",
		"Password for 'https://me%40corp.com@github.com/': ",
		"Password for 'https://evil%2F@github.com': ",
		"Password for 'https://u:p%2F@github.com': ",
	} {
		var out bytes.Buffer
		if code := Askpass(dir, []string{prompt}, &out); code != 0 || out.String() != "SECRET-PAT\n" {
			t.Errorf("%q: code %d, output %q", prompt, code, out.String())
		}
	}
}

func TestGitEnvLookalikeOrigin(t *testing.T) {
	ws := testWorkspace(t)
	cred := &runnerapi.RunnerCredential{Name: "scm", Inputs: map[string]string{"username": "u", "password": "p"}}
	for _, u := range []string{"https://gİthub.com/x.git", "https://g%C4%B0thub.com/x.git"} {
		_, cleanup, err := gitEnv(Config{Askpass: "/bin/dispatch-runner"}, ws, &runnerapi.RunnerProject{Url: u, Credential: cred})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(ws.secret("scm"), "origin")); len(got) != 0 {
			t.Errorf("%s: origin %q, want none", u, got)
		}
		cleanup()
	}
	if o := urlOrigin(&url.URL{Scheme: "https", Host: "gİthub.com"}); o != "" {
		t.Errorf("urlOrigin = %q", o)
	}
}

func TestWithGitConfig(t *testing.T) {
	cfg := [][2]string{{"a.b", "1"}, {"c.d", "2"}}
	got := withGitConfig([]string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_VALUE_0=z",
		"GIT_CONFIG_KEY_1=stale", "GIT_CONFIG_VALUE_1=stale"}, cfg)
	want := []string{"PATH=/bin", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_VALUE_0=z",
		"GIT_CONFIG_KEY_1=a.b", "GIT_CONFIG_VALUE_1=1", "GIT_CONFIG_KEY_2=c.d", "GIT_CONFIG_VALUE_2=2", "GIT_CONFIG_COUNT=3"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got = withGitConfig([]string{"GIT_CONFIG_COUNT=bogus"}, cfg[:1])
	if want := []string{"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1", "GIT_CONFIG_COUNT=1"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestGitPromptsAreSanitized runs the real git against a server that asks
// for a password, with a user config that turns credential.sanitizePrompt
// off: the runner's environment turns it back on, and Askpass answers none
// of the prompts for a URL whose user name holds an encoded /.
func TestGitPromptsAreSanitized(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tmp := t.TempDir()
	global := filepath.Join(tmp, "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\tsanitizePrompt = false\n\tuseHttpPath = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	prompts := filepath.Join(tmp, "prompts")
	script := filepath.Join(tmp, "askpass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+prompts+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ws := testWorkspace(t)
	cred := &runnerapi.RunnerCredential{Name: "scm", Inputs: map[string]string{"username": "u", "password": "p"}}
	// The project is on "victim"; a submodule names victim%2F@<our server>.
	env, cleanup, err := gitEnv(Config{Askpass: script}, ws, &runnerapi.RunnerProject{Url: "http://victim/r.git", Credential: cred})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-remote", "http://victim%2F@"+host+"/r.git")
	cmd.Env = env
	cmd.Dir = tmp
	_ = cmd.Run()

	b, err := os.ReadFile(prompts)
	if err != nil {
		t.Fatalf("git asked nothing: %v", err)
	}
	dir := askpassDir(t, "http://victim:80")
	for _, p := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(p, "victim%2F@") {
			t.Errorf("prompt %q is not sanitized", p)
		}
		var out bytes.Buffer
		if code := Askpass(dir, []string{p}, &out); code == 0 || out.Len() > 0 {
			t.Errorf("%q: answered %q", p, out.String())
		}
	}
}

func TestPlainPromptURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://me%40corp.com@github.com":  true,
		"http://victim%2F@evil/":            true, // host evil: refused by the origin compare
		"https://host:8443":                 true,
		"http://a@b@evil":                   false,
		"http://victim/@evil":               false,
		"http://victim/x":                   false,
		"http://victim?x":                   false,
		"http://victim#x":                   false,
		"mailto:me@corp.com":                false,
		"http://me%40corp.com@host:1/":      true,
		"http://me%40corp.com@host:1/r.git": false,
		"http://me@corp.com@host:1":         false,
		"http://u:p@ss@host":                false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := plainPromptURL(raw, u); got != want {
			t.Errorf("plainPromptURL(%q) = %v, want %v", raw, got, want)
		}
	}
	if u, _ := url.Parse("http://victim%2F@evil/"); u.Hostname() != "evil" {
		t.Errorf("http://victim%%2F@evil/ has host %q", u.Hostname())
	}
}

// TestGitPromptEmailUser runs the real git with a user name that is an
// email address: git encodes its @ in the prompt, and Askpass answers.
func TestGitPromptEmailUser(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	prompts := filepath.Join(tmp, "prompts")
	script := filepath.Join(tmp, "askpass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+prompts+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ws := testWorkspace(t)
	project := "http://me%40corp.com@" + host + "/r.git"
	cred := &runnerapi.RunnerCredential{Name: "scm", Inputs: map[string]string{"username": "me@corp.com", "password": "p"}}
	env, cleanup, err := gitEnv(Config{Askpass: script}, ws, &runnerapi.RunnerProject{Url: project, Credential: cred})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-remote", project)
	cmd.Env = env
	cmd.Dir = tmp
	_ = cmd.Run()

	b, err := os.ReadFile(prompts)
	if err != nil {
		t.Fatalf("git asked nothing: %v", err)
	}
	dir := askpassDir(t, "http://"+host)
	asked := false
	for _, p := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(p, "Password") {
			continue
		}
		asked = true
		var out bytes.Buffer
		if code := Askpass(dir, []string{p}, &out); code != 0 || out.String() != "SECRET-PAT\n" {
			t.Errorf("%q: code %d, output %q", p, code, out.String())
		}
	}
	if !asked {
		t.Errorf("git asked no password: %q", b)
	}
}
