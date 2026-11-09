package runner

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hardenChildEnv makes the test binary act as a runner that has (or
// hasn't) hardened itself, holding a token in its environment.
const hardenChildEnv = "DISPATCH_TEST_HARDEN_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(hardenChildEnv); mode != "" {
		if mode == "harden" {
			if err := Harden(); err != nil {
				fmt.Println("error:", err)
				os.Exit(1)
			}
		}
		fmt.Println("ready")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// L8: once hardened, another process of the same user (the job's code)
// can't read the runner's environment, where its token is.
func TestHardenHidesTheToken(t *testing.T) {
	const secret = "dispatch_job_canary_token"
	environ := func(mode string) (string, error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), hardenChildEnv+"="+mode, JobTokenEnv+"="+secret)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		line, _ := bufio.NewReader(out).ReadString('\n')
		if strings.TrimSpace(line) != "ready" {
			t.Fatalf("child: %q", line)
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", cmd.Process.Pid))
		return string(b), err
	}

	plain, err := environ("plain")
	if err != nil || !strings.Contains(plain, secret) {
		t.Skipf("can't read another process's environment here even without hardening (%v)", err)
	}
	hardened, err := environ("harden")
	if err == nil {
		if os.Geteuid() == 0 {
			t.Skip("running as root, which may read any process's environment")
		}
		t.Fatalf("read a hardened runner's environment: %d bytes, token visible: %v", len(hardened), strings.Contains(hardened, secret))
	}
	if !os.IsPermission(err) {
		t.Errorf("reading a hardened runner's environment: %v, want permission denied", err)
	}
}
