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
