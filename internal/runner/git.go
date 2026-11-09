package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"dispatch-ops/internal/runner/runnerapi"
)

// gitProtocols are the transports git may use: no remote helpers such as
// ext::, which run commands. The API accepts project URLs only in these.
const gitProtocols = "https:http:ssh:git:file"

// AskpassDirEnv tells the runner binary to act as GIT_ASKPASS: it answers
// git's prompt from the files in this directory. See Askpass.
const AskpassDirEnv = "DISPATCH_ASKPASS_DIR"

// Askpass answers a git credential prompt (git passes it as the only
// argument) from the username and password files in dir. It returns the
// process exit code.
//
// It answers only for the project's own server, whose origin is in the
// file "origin" in dir: git's prompt names the URL it wants a credential
// for (Username for 'https://host': , Password for 'https://user@host': ).
// Git also runs it for submodules, and ansible-galaxy's git for roles and
// collections from git, whose URLs come from the repository; without the
// check, a repository writer could point one at their own server and
// collect the credential.
func Askpass(dir string, args []string, out io.Writer) int {
	if len(args) == 0 {
		return 1
	}
	prompt := args[0]
	var file string
	switch lower := strings.ToLower(prompt); {
	case strings.HasPrefix(lower, "username"):
		file = "username"
	case strings.HasPrefix(lower, "password"):
		file = "password"
	default:
		return 1
	}
	want, err := os.ReadFile(filepath.Join(dir, "origin"))
	if err != nil || string(want) == "" {
		return 1
	}
	start, end := strings.Index(prompt, "'"), strings.LastIndex(prompt, "'")
	if start < 0 || end <= start {
		return 1
	}
	raw := prompt[start+1 : end]
	asked, err := url.Parse(raw)
	if err != nil || !plainPromptURL(raw, asked) || urlOrigin(asked) != string(want) {
		fmt.Fprintf(os.Stderr, "dispatch-runner: not sending the project's credential to %s, which is not the project's server\n",
			redactURL(prompt[start+1:end]))
		return 1
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 1
	}
	fmt.Fprintln(out, string(b))
	return 0
}

// plainPromptURL reports whether raw, which parses as u, looks like a URL
// git names in a credential prompt (scheme://[user@]host[:port], see
// gitEnv's credential.useHttpPath=false), with nothing in it that could
// make Go and git disagree about the host: more than one literal @ in the
// authority, or a path, query or fragment. Without
// credential.sanitizePrompt, git would show http://victim%2F@evil/ as
// http://victim/@evil, which Go reads as host victim. Encoded characters
// in the user name (me%40corp.com, victim%2F) are fine: they can't move
// the host, which is compared to the project's afterwards.
func plainPromptURL(raw string, u *url.URL) bool {
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return false
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	return strings.Count(authority, "@") <= 1 && (tail == "" || tail == "/")
}

// checkout clones the project into ws.projectDir at its ref and returns the
// commit it checked out. Git's output goes to the log as runner messages.
func checkout(ctx context.Context, cfg Config, ws *workspace, p *runnerapi.RunnerProject, ev *emitter) (string, error) {
	ev.runner(fmt.Sprintf("Checking out %s at %s", redactURL(p.Url), p.Ref))
	env, cleanup, err := gitEnv(cfg, ws, p)
	defer cleanup()
	if err != nil {
		return "", err
	}
	git := func(args ...string) (string, error) {
		return runGit(ctx, ws.projectDir, env, args...)
	}
	if err := os.MkdirAll(ws.projectDir, 0o700); err != nil {
		return "", err
	}
	if _, err := git("init", "-q"); err != nil {
		return "", err
	}
	if _, err := git("remote", "add", "origin", p.Url); err != nil {
		return "", err
	}
	// A shallow fetch of the ref: a branch, a tag, or a commit if the server
	// allows fetching commits directly.
	target := "FETCH_HEAD"
	if _, err := git("fetch", "-q", "--depth", "1", "origin", p.Ref); err != nil {
		if !isCommitID(p.Ref) || ctx.Err() != nil {
			return "", err
		}
		ev.runner("The server can't fetch a single commit; fetching the whole repository")
		if _, err := git("fetch", "-q", "--tags", "origin"); err != nil {
			return "", err
		}
		target = p.Ref
	}
	if _, err := git("checkout", "-q", "--detach", target); err != nil {
		return "", err
	}
	rev, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(ws.projectDir, ".gitmodules")); err == nil {
		ev.runner("Updating submodules")
		if _, err := git("submodule", "update", "-q", "--init", "--recursive", "--depth", "1"); err != nil {
			return rev, err
		}
	}
	ev.runner("Checked out commit " + rev)
	return rev, nil
}

var commitID = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func isCommitID(ref string) bool { return commitID.MatchString(ref) }

// urlOrigin is where git sends a credential for u: its scheme, host and
// port, with the scheme's default port filled in. It is "" if the host
// isn't ASCII once percent-decoded (see asciiHost), so that a look-alike
// host never matches.
func urlOrigin(u *url.URL) string {
	host, ok := asciiHost(u.Hostname())
	if !ok || host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[scheme]
	}
	return scheme + "://" + host + ":" + port
}

// asciiHost returns host percent-decoded (repeatedly, as git or a server
// might decode it) and lower-cased. ok is false if the result isn't
// printable ASCII, or doesn't decode. Go lower-cases the look-alike
// gİthub.com to github.com, while git connects to xn--github-qyd.com.
func asciiHost(host string) (string, bool) {
	for i := 0; ; i++ {
		d, err := url.PathUnescape(host)
		if err != nil || i == 4 {
			return "", false
		}
		if d == host {
			break
		}
		host = d
	}
	for i := 0; i < len(host); i++ {
		if host[i] <= 0x20 || host[i] >= 0x7f {
			return "", false
		}
	}
	return strings.ToLower(host), true
}

// gitConfig is configuration every git the runner starts runs with, git's
// own and ansible-galaxy's: prompts name the server without a path and
// with URL-encoded user names, so Askpass reads the host git means.
var gitConfig = [][2]string{
	{"credential.sanitizePrompt", "true"},
	{"credential.useHttpPath", "false"},
}

// withGitConfig adds config to env through GIT_CONFIG_COUNT,
// GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>, after any entries env
// already has, and in place of a malformed count. Such configuration
// overrides the system, user and repository files.
func withGitConfig(env []string, config [][2]string) []string {
	n := 0
	out := make([]string, 0, len(env)+2*len(config)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c >= 0 && c < 1000 {
				n = c
			}
			continue
		}
		out = append(out, kv)
	}
	// Entries past the count would be ignored, but drop any that ours
	// would duplicate.
	out = slices.DeleteFunc(out, func(kv string) bool {
		for i := n; i < n+len(config); i++ {
			if strings.HasPrefix(kv, fmt.Sprintf("GIT_CONFIG_KEY_%d=", i)) || strings.HasPrefix(kv, fmt.Sprintf("GIT_CONFIG_VALUE_%d=", i)) {
				return true
			}
		}
		return false
	})
	for i, c := range config {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, c[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, c[1]))
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+len(config)))
}

// gitEnv returns git's environment, with the credential of the project p
// (if any) set up for p's server only. The returned cleanup removes the
// credential's files, so the job's command can't read them.
func gitEnv(cfg Config, ws *workspace, p *runnerapi.RunnerProject) (env []string, cleanup func(), err error) {
	var cred *runnerapi.RunnerCredential
	if p != nil {
		cred = p.Credential
	}
	dir := ws.secret("scm")
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cleanup, err
	}
	env = append(withGitConfig(baseEnv(), gitConfig),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+gitProtocols,
	)
	// Job pods start without known hosts: trust the key seen first, unless
	// the credential lists the server's keys, which are then required.
	hostKeys := []string{"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + shellQuote(ws.path("known_hosts"))}
	if cred != nil && strings.TrimSpace(cred.Inputs["known_hosts"]) != "" {
		kh := cred.Inputs["known_hosts"]
		if !strings.HasSuffix(kh, "\n") {
			kh += "\n"
		}
		p := filepath.Join(dir, "known_hosts")
		if err := os.WriteFile(p, []byte(kh), 0o600); err != nil {
			return nil, cleanup, err
		}
		hostKeys = []string{"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + shellQuote(p)}
	}
	sshCmd := append([]string{"ssh", "-o", "BatchMode=yes"}, hostKeys...)

	if cred != nil {
		in := cred.Inputs
		if in["username"] != "" || in["password"] != "" {
			// Askpass answers only for this origin; an unparsable URL gets none.
			origin := ""
			if u, err := url.Parse(p.Url); err == nil && strings.Contains(p.Url, "://") {
				origin = urlOrigin(u)
			}
			files := map[string]string{"username": in["username"], "password": in["password"], "origin": origin}
			for f, v := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o600); err != nil {
					return nil, cleanup, err
				}
			}
			if cfg.Askpass != "" {
				env = append(env, "GIT_ASKPASS="+cfg.Askpass, AskpassDirEnv+"="+dir)
			}
		}
		if in["private_key"] != "" {
			key := filepath.Join(dir, "id_key")
			if err := writePrivateKey(key, in["private_key"], in["passphrase"]); err != nil {
				return nil, cleanup, fmt.Errorf("credential %q: %w", cred.Name, err)
			}
			sshCmd = append(sshCmd, "-o", "IdentitiesOnly=yes", "-i", shellQuote(key))
		}
	}
	env = append(env, "GIT_SSH_COMMAND="+strings.Join(sshCmd, " "))
	return env, cleanup, nil
}

// runGit runs a git command in dir and returns its trimmed output. Its error
// includes git's output.
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	// Stopping git stops the ssh or HTTP helper it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err == nil {
		return text, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", errors.New("git is not installed in the execution environment image")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if text == "" {
		text = err.Error()
	}
	return "", fmt.Errorf("git %s: %s", args[0], text)
}

// writePrivateKey writes an SSH private key to path, decrypting it first if
// it has a passphrase, so that ssh never needs to ask for one.
func writePrivateKey(path, key, passphrase string) error {
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	data := []byte(key)
	if passphrase != "" {
		raw, err := ssh.ParseRawPrivateKeyWithPassphrase(data, []byte(passphrase))
		switch {
		case errors.Is(err, x509.IncorrectPasswordError):
			return errors.New("the private key's passphrase is wrong")
		case err != nil && (strings.Contains(err.Error(), "not password protected") ||
			strings.Contains(err.Error(), "not an encrypted key")):
			// The key has no passphrase; use it as is.
		case err != nil:
			return fmt.Errorf("read the private key: %w", err)
		default:
			if k, ok := raw.(*ed25519.PrivateKey); ok {
				raw = *k // MarshalPrivateKey takes ed25519 keys by value
			}
			block, err := ssh.MarshalPrivateKey(raw, "")
			if err != nil {
				return fmt.Errorf("re-encode the private key: %w", err)
			}
			data = pem.EncodeToMemory(block)
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// redactURL hides a password in a URL, for log messages.
func redactURL(u string) string {
	if !strings.Contains(u, "://") {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	return parsed.Redacted()
}

// shellQuote quotes s for sh, which runs GIT_SSH_COMMAND.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
