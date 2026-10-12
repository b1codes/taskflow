package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

// DefaultCommandTimeout limits each command check when config sets none.
const DefaultCommandTimeout = 300 * time.Second

// maxCommandOutput is how much of a command's output is kept: its end, where
// test runners print their summary.
const maxCommandOutput = 8 << 10

// commandWaitDelay bounds how long a killed command's leftover children may
// hold its output open.
const commandWaitDelay = 5 * time.Second

// RunCommands runs each command check with sh -c in its stack root under the
// session's project, with the stack's env merged over the inherited
// environment, stdin closed, and the evaluator's time limit. It reports a
// failed check rather than an error for anything that goes wrong. Each
// check's Fingerprint is the state the runs left behind (see
// runSnapshot.stamp, completeness.go).
func (e *CompletenessEvaluator) RunCommands(ctx context.Context, sess *session.Session, specs []session.CommandSpec) []session.CommandCheck {
	snap := e.snapshot(sess.ProjectPath, specs)
	out := make([]session.CommandCheck, 0, len(specs))
	for _, spec := range specs {
		out = append(out, runCommand(ctx, sess.ProjectPath, spec, e.timeout()))
	}
	snap.stamp(specs, out)
	return out
}

func (e *CompletenessEvaluator) timeout() time.Duration {
	if e.commandTimeout > 0 {
		return e.commandTimeout
	}
	return DefaultCommandTimeout
}

func runCommand(ctx context.Context, projectPath string, spec session.CommandSpec, timeout time.Duration) session.CommandCheck {
	check := session.CommandCheck{Command: spec.Line, Root: spec.Root, RanAt: time.Now()}
	fail := func(err string) session.CommandCheck {
		check.ExitCode, check.Error = -1, err
		return check
	}
	dir := filepath.Join(projectPath, filepath.FromSlash(spec.Root))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fail(fmt.Sprintf("stack root %q is not a directory", spec.Root))
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := shellCommand(ctx, spec.Line)
	if err != nil {
		return fail(err.Error())
	}
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), spec.Env)
	buf := &tailBuffer{max: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.WaitDelay = commandWaitDelay

	err = cmd.Run()
	check.Output = buf.String()
	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		check.TimedOut = true
		return fail(fmt.Sprintf("timed out after %s", timeout))
	case err == nil:
		check.Passed = true
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		check.ExitCode = exitErr.ExitCode()
	default:
		return fail(err.Error())
	}
	return check
}

// mergeEnv returns base with env set over it. exec uses the last value of a
// duplicated key, so env is appended (sorted, for a stable order).
func mergeEnv(base []string, env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := append([]string(nil), base...)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max   int
	buf   []byte
	total int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

// String is the kept output, noting how much was cut from its start. A rune
// split by the cut is dropped.
func (b *tailBuffer) String() string {
	data := b.buf
	if len(data) > b.max {
		data = data[len(data)-b.max:]
	}
	s := strings.ToValidUTF8(string(data), "")
	if cut := b.total - len(data); cut > 0 {
		s = fmt.Sprintf("…(%d bytes cut)\n", cut) + s
	}
	return s
}
