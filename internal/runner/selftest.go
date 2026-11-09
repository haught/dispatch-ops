package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"dispatch-ops/internal/runner/runnerapi"
)

// selfTestEnv is what SelfTest inspects, so tests can stand in for the image.
type selfTestEnv struct {
	uid      int
	lookPath func(string) (string, error)
	// collections returns the installed Ansible collections as
	// "namespace.name version" lines.
	collections func() ([]string, error)
}

// SelfTest checks that the image it runs in can run Dispatch Ops jobs, and
// prints what it finds to w: `docker run --rm <image> selftest`, for CI of
// custom execution environments. It returns the process exit code: 0 when
// every required check passes, 1 when any fails.
func SelfTest(w io.Writer, version string) int {
	return selfTest(w, version, selfTestEnv{
		uid:         os.Getuid(),
		lookPath:    exec.LookPath,
		collections: ansibleCollections,
	})
}

func selfTest(w io.Writer, version string, env selfTestEnv) int {
	failed := false
	check := func(status, name, detail string) {
		if status == "FAIL" {
			failed = true
		}
		fmt.Fprintf(w, "  %-4s  %-18s %s\n", status, name, detail)
	}
	tool := func(name, required, missing string) {
		if p, err := env.lookPath(name); err == nil {
			check("ok", name, p)
		} else {
			check(required, name, missing)
		}
	}

	fmt.Fprintf(w, "dispatch-runner %s, runner protocol %d\n", version, runnerapi.ProtocolVersion)
	fmt.Fprintf(w, "  (ansible and script jobs need protocol 2 or later, inventory syncs 3)\n\n")

	if env.uid == 0 {
		check("FAIL", "user", "runs as root; job pods run as non-root, so end the Dockerfile with USER 1000:1000")
	} else {
		check("ok", "user", fmt.Sprintf("uid %d", env.uid))
	}
	_, err := env.lookPath("ansible-playbook")
	hasAnsible := err == nil
	// Every job type but container clones a project, and an image with
	// Ansible is meant for ansible jobs and inventory syncs.
	if hasAnsible {
		tool("git", "FAIL", "not found; ansible jobs and inventory syncs clone their project with git")
	} else {
		tool("git", "warn", "not found; fine for container jobs, but script jobs clone their project with git")
	}
	tool("ssh", "warn", "not found; needed for SSH Git URLs and Ansible's ssh connection")

	if !hasAnsible {
		check("--", "ansible", "ansible-playbook not found; this image can't run ansible jobs or inventory syncs")
	} else {
		for _, t := range []string{"ansible-playbook", "ansible-inventory", "ansible-galaxy"} {
			tool(t, "FAIL", "not found; ansible jobs and inventory syncs need it")
		}
		cols, err := env.collections()
		switch {
		case err != nil:
			check("FAIL", "collections", err.Error())
		case len(cols) == 0:
			check("ok", "collections", "none installed")
		default:
			check("ok", "collections", fmt.Sprintf("%d installed:", len(cols)))
			for _, c := range cols {
				fmt.Fprintf(w, "  %-4s  %-18s   %s\n", "", "", c)
			}
		}
	}

	if failed {
		fmt.Fprintln(w, "\nFAIL: this image can't run Dispatch Ops jobs")
		return 1
	}
	fmt.Fprintln(w, "\nok")
	return 0
}

// ansibleCollections lists the collections ansible-galaxy finds in the
// default search paths.
func ansibleCollections() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ansible-galaxy", "collection", "list", "--format", "json")
	cmd.Env = append(os.Environ(), "ANSIBLE_NOCOLOR=True")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ansible-galaxy collection list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ansible-galaxy collection list: %w", err)
	}
	return parseCollectionList(out)
}

// parseCollectionList reads `ansible-galaxy collection list --format json`:
// {"<path>": {"<namespace.name>": {"version": "<v>"}}}, and returns each
// distinct "namespace.name version", sorted.
func parseCollectionList(out []byte) ([]string, error) {
	var paths map[string]map[string]struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &paths); err != nil {
		return nil, fmt.Errorf("ansible-galaxy collection list: %w", err)
	}
	var cols []string
	for _, cs := range paths {
		for name, c := range cs {
			cols = append(cols, name+" "+c.Version)
		}
	}
	slices.Sort(cols)
	return slices.Compact(cols), nil
}
