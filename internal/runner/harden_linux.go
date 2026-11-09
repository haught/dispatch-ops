package runner

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Harden keeps the job's own code away from the runner's secrets. The job's
// commands run as the runner's user, which could otherwise read the runner
// token from /proc/<pid>/environ or the runner's memory, and use it to
// forge events, report a false result or push an inventory. A non-dumpable
// process's /proc files belong to root and it can't be traced without
// CAP_SYS_PTRACE, which job pods drop. It must be called before the runner
// starts any other process.
func Harden() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("make the runner non-dumpable: %w", err)
	}
	if d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || d != 0 {
		return fmt.Errorf("the runner is still dumpable (%d, %v)", d, err)
	}
	return nil
}
