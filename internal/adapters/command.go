package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// commandWaitDelay bounds how long a killed shell may take to exit, and how
// long anything that escaped its process group may then hold its output open.
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

// runCommand runs one check. The inherited environment loses taskflow's own
// credentials (inheritedEnvDenylist), and whatever the shell leaves running
// in its process group is killed once it exits.
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
	cmd.Env = mergeEnv(inheritedEnv(os.Environ()), spec.Env)
	cmd.WaitDelay = commandWaitDelay

	// The output goes to a pipe of our own rather than one exec copies from,
	// so Wait returns as soon as the shell exits instead of waiting for
	// whatever it left running in the background to let go of the pipe.
	r, w, err := os.Pipe()
	if err != nil {
		return fail(err.Error())
	}
	defer r.Close()
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		return fail(err.Error())
	}
	buf := &tailBuffer{max: maxCommandOutput}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(buf, r)
	}()

	err = cmd.Wait()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	// The shell is gone: anything it left behind in its process group (a
	// backgrounded server, a watcher) goes with it, so it neither leaks nor
	// holds the output open.
	killGroup(cmd)
	select {
	case <-copied:
	case <-time.After(commandWaitDelay):
		// Something that left the group still holds the pipe: keep what
		// it has written so far.
		_ = r.SetReadDeadline(time.Now())
		<-copied
	}
	check.Output = buf.String()

	var exitErr *exec.ExitError
	switch {
	case timedOut:
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

// inheritedEnvDenylist names taskflow's own credentials, which a command
// check doesn't get from taskflow's environment: its command line comes from
// a manifest the agent can edit. A stack's env: may still set them on
// purpose.
var inheritedEnvDenylist = []string{
	"CLICKUP_API_KEY", // ClickUp token (internal/config)
}

// inheritedEnv returns environ without the variables in inheritedEnvDenylist.
func inheritedEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if key, _, _ := strings.Cut(kv, "="); !slices.Contains(inheritedEnvDenylist, key) {
			out = append(out, kv)
		}
	}
	return out
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
