//go:build unix

package adapters

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

// waitGone reports whether the process whose pid is in pidFile is gone
// within d, killing it if not.
func waitGone(t *testing.T, pidFile string, d time.Duration) bool {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(d); ; {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunCommands_LeftoverChildren(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for i, tc := range []struct {
		name, line string
		passed     bool
		exitCode   int
	}{
		{"holding stdout after exit 0 passes", `sleep 60 & echo $! > pid; echo ok`, true, 0},
		{"holding stderr after exit 0 passes", `sleep 60 >/dev/null & echo $! > pid; echo ok`, true, 0},
		{"holding stdout after a failure keeps its code", `sleep 60 & echo $! > pid; echo ok; exit 4`, false, 4},
	} {
		root := fmt.Sprintf("case%d", i)
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := os.MkdirAll(filepath.Join(dir, root), 0o755); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			c := run(t, dir, session.CommandSpec{Line: tc.line, Root: root}, time.Minute)
			if elapsed := time.Since(began); elapsed > 2*time.Second {
				t.Errorf("took %s: waited on the background child", elapsed)
			}
			if c.Passed != tc.passed || c.ExitCode != tc.exitCode || c.TimedOut || c.Error != "" || strings.TrimSpace(c.Output) != "ok" {
				t.Errorf("check = %+v", c)
			}
			if !waitGone(t, filepath.Join(dir, root, "pid"), 2*time.Second) {
				t.Error("the background child outlived the command")
			}
		})
	}

	t.Run("a timeout still times out and kills the group", func(t *testing.T) {
		t.Parallel()
		if err := os.MkdirAll(filepath.Join(dir, "timeout"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := run(t, dir, session.CommandSpec{Line: `sleep 60 & echo $! > pid; echo started; sleep 60`, Root: "timeout"}, 300*time.Millisecond)
		if c.Passed || !c.TimedOut || c.ExitCode != -1 || !strings.Contains(c.Error, "timed out after 300ms") || !strings.Contains(c.Output, "started") {
			t.Errorf("check = %+v", c)
		}
		if !waitGone(t, filepath.Join(dir, "timeout", "pid"), 2*time.Second) {
			t.Error("the background child outlived the timeout")
		}
	})
}

func TestRunCommands_StripsTaskflowCredentials(t *testing.T) {
	// t.Setenv: not parallel.
	t.Setenv("CLICKUP_API_KEY", "pk_secret")
	t.Setenv("TASKFLOW_TEST_KEEP", "kept")
	dir := t.TempDir()

	c := run(t, dir, session.CommandSpec{Line: `echo "key=${CLICKUP_API_KEY-unset} keep=$TASKFLOW_TEST_KEEP"`, Root: "."}, time.Minute)
	if !c.Passed || strings.TrimSpace(c.Output) != "key=unset keep=kept" {
		t.Errorf("inherited: check = %+v", c)
	}

	// A stack's env: sets it on purpose.
	c = run(t, dir, session.CommandSpec{Line: `echo "key=$CLICKUP_API_KEY"`, Root: ".",
		Env: map[string]string{"CLICKUP_API_KEY": "from-manifest"}}, time.Minute)
	if !c.Passed || strings.TrimSpace(c.Output) != "key=from-manifest" {
		t.Errorf("stack env: check = %+v", c)
	}
}

func TestInheritedEnv(t *testing.T) {
	t.Parallel()
	got := inheritedEnv([]string{"PATH=/bin", "CLICKUP_API_KEY=a", "CLICKUP_API_KEY_OTHER=b", "CLICKUP_API_KEY=c", "X=CLICKUP_API_KEY=d"})
	want := []string{"PATH=/bin", "CLICKUP_API_KEY_OTHER=b", "X=CLICKUP_API_KEY=d"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("inheritedEnv = %q, want %q", got, want)
	}
}
