//go:build unix

package adapters

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs line with /bin/sh in its own process group, so a timeout
// kills everything it started (a package manager's test runner, say), not
// just the shell.
func shellCommand(ctx context.Context, line string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd, nil
}

// killGroup kills what is left of cmd's process group once its shell has
// exited. The group's ID can't be taken by another process while any member
// is alive; once none is, the kill finds nothing (ESRCH), short of the pid
// being reused in the moment since the shell was reaped.
func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
