package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/store"
)

// An agent's command checks through MCP: tf_verify runs the suite on demand,
// and tf_stop runs it again before the block gate decides.
func TestMCPServer_Verify(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping: git init failed")
	}
	manifest := `version: 1
gate: block
stacks:
  - { id: app, root: ".", commands: { test: [{ run: "sh ./suite.sh" }] } }
rules:
  - id: regression-suite
    stack: app
    describe: "Existing {stack} test suite still passes"
    scope: stack
    when: { changed: "**" }
    require:
      - { id: suite, describe: "Full {stack} suite passes", run: { command: test }, attest: "Run {command}", attest_when: no_command }
`
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".taskflow/completeness.yaml", manifest)
	write("suite.sh", "echo 'FAIL app.test'; exit 1\n")

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	t.Run("off: refused, and the suite asks for an attestation", func(t *testing.T) {
		srv := mustNew(t, st, &config.Config{})
		contract := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-off", "project_path": dir})
		sid := contract["session_id"].(string)
		if text := callToolError(t, "tf_verify", srv.handleVerify, map[string]any{"session_id": sid}); !strings.Contains(text, "allow_commands") {
			t.Errorf("tf_verify error = %s", text)
		}
		callTool(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid, "status": "archived"})
	})

	srv := mustNew(t, st, &config.Config{Completeness: config.CompletenessConfig{AllowCommands: true, CommandTimeoutS: 60}})
	contract := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-on", "project_path": dir})
	sid := contract["session_id"].(string)
	cc := contract["completeness"].(map[string]any)
	if cc["commands_enabled"] != true || !strings.Contains(cc["protocol"].(string), "tf_verify") {
		t.Errorf("contract completeness = %v", cc)
	}

	report := callTool(t, "tf_verify", srv.handleVerify, map[string]any{"session_id": sid})
	runs, _ := report["commands"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["passed"] != false {
		t.Fatalf("tf_verify report = %v", report)
	}
	text := callToolError(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid})
	if !strings.Contains(text, "Full app suite passes") || !strings.Contains(text, "command `sh ./suite.sh` in .: failed (exit 1): FAIL app.test") {
		t.Errorf("tf_stop under block with a failing suite = %s", text)
	}

	write("suite.sh", "echo 'ok'\n")
	report = callTool(t, "tf_verify", srv.handleVerify, map[string]any{"session_id": sid})
	if sum := report["summary"].(map[string]any); sum["open"] != float64(0) || sum["satisfied"] != float64(1) {
		t.Errorf("after the fix: %v", report)
	}
	stop := callTool(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid})
	if stop["new_status"] != "COMPLETED" || stop["gate"].(map[string]any)["outcome"] != "passed" {
		t.Errorf("tf_stop = %v", stop)
	}
}
