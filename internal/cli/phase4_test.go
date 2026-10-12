package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

// startDoDProject starts a session in a git project whose manifest asks for
// a test per new .go file, and adds widget.go, so one required obligation
// is open. It returns the project and session ID.
func startDoDProject(t *testing.T, tempHome, manifest string) (string, string) {
	t.Helper()
	projDir := filepath.Join(tempHome, "dod-project")
	if manifest == "" {
		manifest = "version: 1\nrules:\n  - id: r\n    when: { added: \"{name}.go\", exclude: [\"*_test.go\"] }\n    require:\n      - { id: test, describe: \"Test for {name}\", verify: { exists: \"{name}_test.go\" } }\n"
	}
	if err := os.MkdirAll(filepath.Join(projDir, ".taskflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", projDir, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Skipf("skipping: git init failed: %s", out)
	}
	out, err := runCLI(t, []string{"start", "task-cli-p4", projDir, "--json"})
	if err != nil {
		t.Fatalf("start failed: %v, output: %s", err, out)
	}
	var contract session.AgenticContract
	if err := json.Unmarshal([]byte(out), &contract); err != nil {
		t.Fatalf("invalid start JSON: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(projDir, "widget.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projDir, contract.SessionID
}

func exitCode(err error) int {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	if err != nil {
		return 1
	}
	return 0
}

func TestCLI_Check(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir, sid := startDoDProject(t, tempHome, "")

	out, err := runCLI(t, []string{"check", "--project", projDir})
	if exitCode(err) != checkBlockedExit {
		t.Fatalf("check with open work: err = %v, want exit 2; output: %s", err, out)
	}
	if !strings.Contains(out, "1 required obligation(s) open") || !strings.Contains(out, "Test for widget") || !strings.Contains(out, "tf_waive") {
		t.Errorf("check output = %s", out)
	}

	out, err = runCLI(t, []string{"check", "--session", sid, "--json"})
	var res session.CheckResult
	if exitCode(err) != 2 || json.Unmarshal([]byte(out), &res) != nil || !res.Blocked || len(res.OpenRequired) != 1 {
		t.Errorf("check --json = %s, %v", out, err)
	}

	t.Run("claude hook blocks with the open list, and exits 0", func(t *testing.T) {
		rootCmd.SetIn(strings.NewReader(`{"session_id":"cc","hook_event_name":"Stop","stop_hook_active":false,"cwd":"` + projDir + `"}`))
		defer rootCmd.SetIn(nil)
		out, err := runCLI(t, []string{"check", "--hook", "claude"})
		if err != nil {
			t.Fatalf("hook should exit 0: %v", err)
		}
		var resp claudeHookOutput
		if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.Decision != "block" || !strings.Contains(resp.Reason, "Test for widget") {
			t.Errorf("hook output = %q, %v", out, err)
		}
	})

	t.Run("claude hook allows the stop when it is already active", func(t *testing.T) {
		rootCmd.SetIn(strings.NewReader(`{"stop_hook_active":true,"cwd":"` + projDir + `"}`))
		defer rootCmd.SetIn(nil)
		out, err := runCLI(t, []string{"check", "--hook", "claude"})
		if err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("out = %q, err = %v", out, err)
		}
	})

	t.Run("unknown hook", func(t *testing.T) {
		if _, err := runCLI(t, []string{"check", "--project", projDir, "--hook", "cursor"}); err == nil || !strings.Contains(err.Error(), "unknown --hook") {
			t.Errorf("err = %v", err)
		}
	})

	// Doing the work clears the check.
	if err := os.WriteFile(filepath.Join(projDir, "widget_test.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, []string{"check", "--project", projDir})
	if err != nil || !strings.Contains(out, "no required obligations open") {
		t.Errorf("check after the work: %v, %s", err, out)
	}
	rootCmd.SetIn(strings.NewReader(`{"stop_hook_active":false}`))
	out, err = runCLI(t, []string{"check", "--project", projDir, "--hook", "claude"})
	rootCmd.SetIn(nil)
	if err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("hook with nothing open should say nothing: %q, %v", out, err)
	}

	t.Run("no session in the project", func(t *testing.T) {
		out, err := runCLI(t, []string{"check", "--project", t.TempDir()})
		if err != nil || !strings.Contains(out, "nothing to check") {
			t.Errorf("out = %q, err = %v", out, err)
		}
	})
}

func TestCLI_Verify(t *testing.T) {
	tempHome := setupTestEnv(t)
	manifest := "version: 1\nstacks:\n  - { id: app, root: \".\", commands: { test: [{ run: \"sh ./suite.sh\" }] } }\n" +
		"rules:\n  - id: regression-suite\n    stack: app\n    describe: \"suite\"\n    scope: stack\n    when: { changed: \"**\" }\n" +
		"    require:\n      - { id: suite, describe: \"Full {stack} suite passes\", run: { command: test }, attest: \"Run {command}\", attest_when: no_command }\n"
	projDir, sid := startDoDProject(t, tempHome, manifest)
	if err := os.WriteFile(filepath.Join(projDir, "suite.sh"), []byte("echo 'Tests: 2 failed'; exit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, []string{"verify", sid})
	if !errors.Is(err, session.ErrCommandsDisabled) {
		t.Fatalf("verify with commands off: err = %v", err)
	}

	cfgPath := filepath.Join(tempHome, ".taskflow", "config.toml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	enabled := strings.Replace(string(cfg), "allow_commands = false", "allow_commands = true", 1)
	if enabled == string(cfg) {
		t.Fatal("default config has no allow_commands = false line")
	}
	if err := os.WriteFile(cfgPath, []byte(enabled), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, []string{"verify", sid})
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	for _, want := range []string{"○ Full app suite passes", "command: `sh ./suite.sh` in .: failed (exit 1)", "| Tests: 2 failed", "✗ ran `sh ./suite.sh` in . (exit 1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output missing %q:\n%s", want, out)
		}
	}

	if err := os.WriteFile(filepath.Join(projDir, "suite.sh"), []byte("echo 'Tests: 2 passed'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, []string{"verify", sid, "--json"})
	var report session.CompletenessReport
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || report.Summary.Satisfied != 1 || len(report.Commands) != 1 || !report.Commands[0].Passed {
		t.Errorf("verify --json = %s, %v", out, err)
	}
	// The recorded pass counts for check, which runs nothing.
	if out, err := runCLI(t, []string{"check", "--session", sid}); err != nil {
		t.Errorf("check after a passing verify: %v\n%s", err, out)
	}
}

func TestCLI_CompletenessReport(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir, sid := startDoDProject(t, tempHome, "")
	if _, err := runCLI(t, []string{"checkpoint", sid, "--summary", "widget"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, []string{"stop", sid, "--status", "completed"}); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, []string{"completeness", "report", "--project", projDir})
	if err != nil || !strings.Contains(out, "r/test") || !strings.Contains(out, "OPEN AT STOP") {
		t.Errorf("report: %v\n%s", err, out)
	}
	out, err = runCLI(t, []string{"completeness", "report", "--format", "json", "--since", "0"})
	var report struct {
		Requirements []struct {
			RuleID     string `json:"rule_id"`
			OpenAtStop int    `json:"open_at_stop"`
		} `json:"requirements"`
	}
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || len(report.Requirements) != 1 || report.Requirements[0].OpenAtStop != 1 {
		t.Errorf("report json: %v\n%s", err, out)
	}
	if out, err := runCLI(t, []string{"completeness", "report", "--format", "b1"}); err != nil || !strings.Contains(out, "No suggestions") {
		t.Errorf("report b1: %v\n%s", err, out)
	}
	if _, err := runCLI(t, []string{"completeness", "report", "--format", "yaml"}); err == nil {
		t.Error("unknown format should fail")
	}
	// The session argument form still works beside the subcommand.
	if out, err := runCLI(t, []string{"completeness", sid}); err != nil || !strings.Contains(out, "Completeness for session") {
		t.Errorf("completeness <session>: %v\n%s", err, out)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":           {},
		"0":          {},
		"30d":        now.Add(-30 * 24 * time.Hour),
		"2w":         now.Add(-14 * 24 * time.Hour),
		"12h":        now.Add(-12 * time.Hour),
		"2026-09-01": time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-3d", "3x"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) should fail", bad)
		}
	}
}
