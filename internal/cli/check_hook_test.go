package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfig replaces ~/.taskflow/config.toml under tempHome.
func writeTestConfig(t *testing.T, tempHome, content string) {
	t.Helper()
	dir := filepath.Join(tempHome, ".taskflow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lockedProject is a project directory whose session lock names sessionID.
func lockedProject(t *testing.T, sessionID string) string {
	t.Helper()
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".b1codes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".b1codes", "session.lock"), []byte(sessionID), 0o644); err != nil {
		t.Fatal(err)
	}
	return proj
}

// unreadableLockProject is a project whose session lock path is a
// directory, so reading it fails with something other than "not found".
func unreadableLockProject(t *testing.T) string {
	t.Helper()
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".b1codes", "session.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	return proj
}

// runHook runs check --hook claude with stdin as the hook input.
func runHook(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	rootCmd.SetIn(strings.NewReader(stdin))
	defer rootCmd.SetIn(nil)
	return runCLI(t, append([]string{"check", "--hook", "claude"}, args...))
}

// wantHookBlock asserts the hook exited 0 with a block decision whose reason
// contains each of want.
func wantHookBlock(t *testing.T, out string, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("the hook should exit 0 with a block decision, got err = %v; output: %s", err, out)
	}
	var resp claudeHookOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("hook output isn't a decision: %q (%v)", out, err)
	}
	if resp.Decision != "block" {
		t.Errorf("decision = %q, want block", resp.Decision)
	}
	for _, w := range want {
		if !strings.Contains(resp.Reason, w) {
			t.Errorf("reason %q doesn't contain %q", resp.Reason, w)
		}
	}
}

func hookInput(cwd string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "stop_hook_active": false, "cwd": cwd})
	return string(b)
}

func TestCLI_CheckHookBlocksOnErrors(t *testing.T) {
	const unchecked = "Taskflow couldn't check this session's completeness"

	t.Run("unreadable session lock", func(t *testing.T) {
		setupTestEnv(t)
		proj := unreadableLockProject(t)
		out, err := runHook(t, hookInput(proj))
		wantHookBlock(t, out, err, unchecked, "failed to read the session lock", "taskflow check")
	})

	t.Run("database that can't be opened", func(t *testing.T) {
		tempHome := setupTestEnv(t)
		notADir := filepath.Join(tempHome, "not-a-dir")
		if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeTestConfig(t, tempHome, "[database]\npath = "+jsonQuote(filepath.Join(notADir, "sessions.db"))+"\n")
		proj := lockedProject(t, "some-session")
		out, err := runHook(t, hookInput(proj))
		wantHookBlock(t, out, err, unchecked, "failed to open database")
	})

	t.Run("invalid config", func(t *testing.T) {
		tempHome := setupTestEnv(t)
		writeTestConfig(t, tempHome, "[completeness]\ngate = \"bogus\"\n")
		proj := lockedProject(t, "some-session")
		out, err := runHook(t, hookInput(proj))
		wantHookBlock(t, out, err, unchecked, "completeness.gate")
	})

	t.Run("invalid hook input", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, `{"stop_hook_active": tru`, "--project", t.TempDir())
		wantHookBlock(t, out, err, unchecked, "invalid hook input")
	})

	t.Run("both --session and --project", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, hookInput(""), "--session", "s", "--project", t.TempDir())
		wantHookBlock(t, out, err, unchecked, "not both")
	})

	t.Run("--session this database doesn't know", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, hookInput(""), "--session", "no-such-session")
		wantHookBlock(t, out, err, unchecked)
	})
}

func TestCLI_CheckHookAllowsWithoutBlocking(t *testing.T) {
	quiet := func(t *testing.T, out string, err error) {
		t.Helper()
		if err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("the hook should allow the stop silently: out = %q, err = %v", out, err)
		}
	}

	t.Run("stop_hook_active wins over an error", func(t *testing.T) {
		tempHome := setupTestEnv(t)
		writeTestConfig(t, tempHome, "[completeness]\ngate = \"bogus\"\n")
		proj := unreadableLockProject(t)
		out, err := runHook(t, `{"stop_hook_active":true,"cwd":`+jsonQuote(proj)+`}`)
		quiet(t, out, err)
	})

	t.Run("no session lock", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, hookInput(t.TempDir()))
		quiet(t, out, err)
	})

	t.Run("lock left by a session this database doesn't know", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, hookInput(lockedProject(t, "unknown-session")))
		quiet(t, out, err)
	})

	t.Run("empty input", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runHook(t, "  \n", "--project", t.TempDir())
		quiet(t, out, err)
	})
}

// Without --hook, failures stay errors (exit 1), distinct from a block's
// exit 2.
func TestCLI_CheckErrorsWithoutHook(t *testing.T) {
	t.Run("unreadable session lock", func(t *testing.T) {
		setupTestEnv(t)
		out, err := runCLI(t, []string{"check", "--project", unreadableLockProject(t)})
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "failed to read the session lock") {
			t.Errorf("err = %v, want a plain error; output: %s", err, out)
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		tempHome := setupTestEnv(t)
		writeTestConfig(t, tempHome, "[completeness]\ngate = \"bogus\"\n")
		out, err := runCLI(t, []string{"check", "--project", t.TempDir()})
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "completeness.gate") {
			t.Errorf("err = %v, want a plain error; output: %s", err, out)
		}
	})
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
