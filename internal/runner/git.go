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
func Askpass(dir string, args []string, out io.Writer) int {
	if len(args) == 0 {
		return 1
	}
	prompt := strings.ToLower(args[0])
	var file string
	switch {
	case strings.HasPrefix(prompt, "username"):
		file = "username"
	case strings.HasPrefix(prompt, "password"):
		file = "password"
	default:
		return 1
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 1
	}
	fmt.Fprintln(out, string(b))
	return 0
}

// checkout clones the project into ws.projectDir at its ref and returns the
// commit it checked out. Git's output goes to the log as runner messages.
func checkout(ctx context.Context, cfg Config, ws *workspace, p *runnerapi.RunnerProject, ev *emitter) (string, error) {
	ev.runner(fmt.Sprintf("Checking out %s at %s", redactURL(p.Url), p.Ref))
	env, cleanup, err := gitEnv(cfg, ws, p.Credential)
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

// gitEnv returns git's environment, with the project credential set up. The
// returned cleanup removes the credential's files, so the job's command
// can't read them.
func gitEnv(cfg Config, ws *workspace, cred *runnerapi.RunnerCredential) (env []string, cleanup func(), err error) {
	dir := ws.secret("scm")
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cleanup, err
	}
	env = append(baseEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+gitProtocols,
	)
	sshCmd := []string{"ssh", "-o", "BatchMode=yes",
		// Job pods start without known hosts; trust the key seen first.
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + shellQuote(ws.path("known_hosts"))}

	if cred != nil {
		in := cred.Inputs
		if in["username"] != "" || in["password"] != "" {
			for _, f := range []string{"username", "password"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(in[f]), 0o600); err != nil {
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
