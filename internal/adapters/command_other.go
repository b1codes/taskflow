//go:build !unix

package adapters

import (
	"context"
	"errors"
	"os/exec"
)

// shellCommand: command checks are command lines for a POSIX shell.
func shellCommand(ctx context.Context, line string) (*exec.Cmd, error) {
	return nil, errors.New("command checks need a POSIX shell (/bin/sh)")
}

// killGroup: shellCommand never starts a command here.
func killGroup(cmd *exec.Cmd) {}
