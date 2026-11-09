// Command dispatch-runner is the entrypoint of every execution environment
// image. The task manager starts it in a job pod; it fetches the job from the
// API, runs it and reports the result. See internal/runner.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"dispatch-ops/internal/runner"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	// `docker run --rm <image> selftest` checks a custom execution environment.
	if len(os.Args) > 1 && os.Args[1] == "selftest" {
		os.Exit(runner.SelfTest(os.Stdout, version))
	}
	// Git runs the runner as GIT_ASKPASS to get a project's username and password.
	if dir := os.Getenv(runner.AskpassDirEnv); dir != "" {
		os.Exit(runner.Askpass(dir, os.Args[1:], os.Stdout))
	}
	cfg, err := runner.ConfigFromEnv(version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch-runner:", err)
		fmt.Fprintln(os.Stderr, "dispatch-runner is started by the Dispatch Ops task manager inside a job pod; it is not meant to be run by hand.")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runner.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "dispatch-runner:", err)
		os.Exit(1)
	}
}
