package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func setupTestEnv(t *testing.T) string {
	tempHome, err := os.MkdirTemp("", "taskflow-cli-test-*")
	if err != nil {
		t.Fatalf("failed to create temp home dir: %v", err)
	}
	t.Setenv("HOME", tempHome)
	return tempHome
}

// resetCobraFlags also resets each command's context: cobra keeps the last
// one (e.g. the cancelled context from the serve test) and only passes the
// root's down to a command that has none yet.
func resetCobraFlags(cmd *cobra.Command) {
	cmd.SetContext(context.Background())
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	for _, child := range cmd.Commands() {
		resetCobraFlags(child)
	}
}

func resetFlags() {
	verbose = false
	jsonOut = false
	checkpointSummary = ""
	checkpointFiles = nil
	snagError = ""
	snagCategory = "build"
	snagResolution = ""
	decisionTopic = ""
	decisionText = ""
	decisionRationale = ""
	decisionStatus = "no_change"
	decisionFiles = nil
	stopSummary = ""
	stopStatus = "completed"
	listStatus = "active,paused"
	listProject = ""
	refreshFlag = false
	checkSession, checkProject, checkHook = "", "", ""
	reportProject, reportSince, reportFormat = "", "30d", "text"
	resetCobraFlags(rootCmd)
	// Set(DefValue) on a string array flag stores "[]" as a value.
	planVars = nil
}

func runCLI(t *testing.T, args []string) (string, error) {
	resetFlags()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	os.Stdout = w

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(args)

	cmdErr := rootCmd.Execute()

	w.Close()
	os.Stdout = oldStdout

	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	outStr := out.String()
	if outStr == "" {
		outStr = buf.String()
	}
	return outStr, cmdErr
}

func TestCLI_Init(t *testing.T) {
	tempHome := setupTestEnv(t)

	out, err := runCLI(t, []string{"init"})
	if err != nil {
		t.Fatalf("init command failed: %v, output: %s", err, out)
	}
	if !strings.Contains(out, "Database initialized at") {
		t.Errorf("expected 'Database initialized at' in output, got: %s", out)
	}

	st, err := store.New(filepath.Join(tempHome, ".taskflow", "sessions.db"))
	if err != nil {
		t.Fatalf("failed to open initialized store: %v", err)
	}
	defer st.Close()
	version, _, err := st.GetSchemaInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSchemaInfo failed: %v", err)
	}
	if want := fmt.Sprintf("Schema version: %d", version); !strings.Contains(out, want) {
		t.Errorf("expected %q in output, got: %s", want, out)
	}
}

func TestCLI_Start_And_Lifecycle(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir := filepath.Join(tempHome, "myproject")
	_ = os.MkdirAll(projDir, 0755)

	// 1. Start missing arguments -> error
	_, err := runCLI(t, []string{"start"})
	if err == nil {
		t.Fatalf("expected error on start with no args, got nil")
	}

	// 2. Start new session (human output)
	out, err := runCLI(t, []string{"start", "task-cli-100", projDir})
	if err != nil {
		t.Fatalf("start failed: %v, output: %s", err, out)
	}
	if !strings.Contains(out, "✓ Session started:") {
		t.Fatalf("expected '✓ Session started:' in output, got: %s", out)
	}
	if !strings.Contains(out, "task-cli-100") {
		t.Errorf("expected task-cli-100 in output, got: %s", out)
	}

	// Extract session ID
	lines := strings.Split(out, "\n")
	var sessionID string
	for _, l := range lines {
		if strings.HasPrefix(l, "✓ Session started: ") {
			sessionID = strings.TrimSpace(strings.TrimPrefix(l, "✓ Session started: "))
			break
		}
	}
	if sessionID == "" {
		t.Fatalf("could not extract session ID from output: %s", out)
	}

	// 3. Start again on active task with --json -> returns JSON contract with Resumed=true
	outJSON, err := runCLI(t, []string{"start", "task-cli-100", projDir, "--json"})
	if err != nil {
		t.Fatalf("start --json failed: %v, output: %s", err, outJSON)
	}
	var contract session.AgenticContract
	if err := json.Unmarshal([]byte(outJSON), &contract); err != nil {
		t.Fatalf("failed to parse JSON output %q: %v", outJSON, err)
	}
	if contract.SessionID != sessionID {
		t.Errorf("expected session ID %s, got %s", sessionID, contract.SessionID)
	}
	if !contract.Resumed {
		t.Errorf("expected contract.Resumed to be true on active start")
	}

	// 4. Checkpoint (human output)
	cpOut, err := runCLI(t, []string{"checkpoint", sessionID, "--summary", "Implemented feature X", "--files", "main.go,cli.go"})
	if err != nil {
		t.Fatalf("checkpoint failed: %v, output: %s", err, cpOut)
	}
	if !strings.Contains(cpOut, "✓ Checkpoint recorded:") {
		t.Errorf("expected '✓ Checkpoint recorded:' in output, got: %s", cpOut)
	}

	// 5. Checkpoint (--json)
	cpJSONOut, err := runCLI(t, []string{"checkpoint", sessionID, "--summary", "JSON checkpoint", "--json"})
	if err != nil {
		t.Fatalf("checkpoint --json failed: %v, output: %s", err, cpJSONOut)
	}
	var cp session.Checkpoint
	if err := json.Unmarshal([]byte(cpJSONOut), &cp); err != nil {
		t.Fatalf("failed to decode checkpoint JSON %q: %v", cpJSONOut, err)
	}
	if cp.Summary != "JSON checkpoint" {
		t.Errorf("expected summary 'JSON checkpoint', got %s", cp.Summary)
	}

	// 6. Checkpoint missing summary -> error
	_, err = runCLI(t, []string{"checkpoint", sessionID})
	if err == nil {
		t.Errorf("expected error on checkpoint without summary")
	}

	// 7. Snag (human output)
	snagOut, err := runCLI(t, []string{"snag", sessionID, "--error", "cannot find symbol Foo", "--category", "build", "--resolution", "imported correct package"})
	if err != nil {
		t.Fatalf("snag failed: %v, output: %s", err, snagOut)
	}
	if !strings.Contains(snagOut, "✓ Snag logged:") {
		t.Errorf("expected '✓ Snag logged:' in output, got: %s", snagOut)
	}

	// 8. Snag (--json)
	snagJSONOut, err := runCLI(t, []string{"snag", sessionID, "--error", "runtime panic", "--category", "runtime", "--json"})
	if err != nil {
		t.Fatalf("snag --json failed: %v, output: %s", err, snagJSONOut)
	}
	var snagRes session.SnagResult
	if err := json.Unmarshal([]byte(snagJSONOut), &snagRes); err != nil {
		t.Fatalf("failed to decode snag JSON %q: %v", snagJSONOut, err)
	}
	if snagRes.Snag.Error != "runtime panic" {
		t.Errorf("expected snag error 'runtime panic', got %s", snagRes.Snag.Error)
	}

	// 9. Snag missing error -> error
	_, err = runCLI(t, []string{"snag", sessionID})
	if err == nil {
		t.Errorf("expected error on snag without --error flag")
	}

	// 10. Decision (human output)
	decOut, err := runCLI(t, []string{"decision", sessionID, "-t", "Architecture", "-d", "Use SQLite", "-r", "Zero config embedded database", "-s", "accepted", "-f", "store/sqlite.go"})
	if err != nil {
		t.Fatalf("decision failed: %v, output: %s", err, decOut)
	}
	if !strings.Contains(decOut, "✓ Decision logged:") {
		t.Errorf("expected '✓ Decision logged:' in output, got: %s", decOut)
	}

	// 11. Decision (--json)
	decJSONOut, err := runCLI(t, []string{"decision", sessionID, "-t", "Sync Retry", "-d", "Exponential backoff", "-r", "Prevent API hammer", "--json"})
	if err != nil {
		t.Fatalf("decision --json failed: %v, output: %s", err, decJSONOut)
	}
	var dec session.Decision
	if err := json.Unmarshal([]byte(decJSONOut), &dec); err != nil {
		t.Fatalf("failed to decode decision JSON %q: %v", decJSONOut, err)
	}
	if dec.Topic != "Sync Retry" {
		t.Errorf("expected topic 'Sync Retry', got %s", dec.Topic)
	}

	// 12. Decision missing required flags -> error
	_, err = runCLI(t, []string{"decision", sessionID, "-t", "Only topic"})
	if err == nil {
		t.Errorf("expected error on decision without decision & rationale")
	}

	// 13. List (human output)
	listOut, err := runCLI(t, []string{"list"})
	if err != nil {
		t.Fatalf("list failed: %v, output: %s", err, listOut)
	}
	if !strings.Contains(listOut, sessionID) {
		t.Errorf("expected session %s in list output, got: %s", sessionID, listOut)
	}

	// 14. List (--json)
	listJSONOut, err := runCLI(t, []string{"list", "--json"})
	if err != nil {
		t.Fatalf("list --json failed: %v, output: %s", err, listJSONOut)
	}
	var summaries []*session.SessionSummary
	if err := json.Unmarshal([]byte(listJSONOut), &summaries); err != nil {
		t.Fatalf("failed to decode list JSON %q: %v", listJSONOut, err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != sessionID {
		t.Errorf("expected 1 summary with session %s, got: %+v", sessionID, summaries)
	}

	// 15. Stop (PAUSED)
	stopOut, err := runCLI(t, []string{"stop", sessionID, "--status", "paused", "--summary", "Pausing for test"})
	if err != nil {
		t.Fatalf("stop paused failed: %v, output: %s", err, stopOut)
	}
	if !strings.Contains(stopOut, "Status set to: PAUSED") {
		t.Errorf("expected 'Status set to: PAUSED' in output, got: %s", stopOut)
	}

	// 16. Stop (COMPLETED with --json)
	stopJSONOut, err := runCLI(t, []string{"stop", sessionID, "--status", "completed", "--summary", "Finished test", "--json"})
	if err != nil {
		t.Fatalf("stop completed --json failed: %v, output: %s", err, stopJSONOut)
	}
	if !strings.Contains(stopJSONOut, `"new_status": "COMPLETED"`) {
		t.Errorf("expected 'new_status': 'COMPLETED' in output, got: %s", stopJSONOut)
	}

	// 17. List after completion (filtered to completed)
	listDoneOut, err := runCLI(t, []string{"list", "--status", "completed", "--json"})
	if err != nil {
		t.Fatalf("list completed failed: %v, output: %s", err, listDoneOut)
	}
	var doneSummaries []*session.SessionSummary
	if err := json.Unmarshal([]byte(listDoneOut), &doneSummaries); err != nil {
		t.Fatalf("failed to decode done summaries %q: %v", listDoneOut, err)
	}
	if len(doneSummaries) != 1 || doneSummaries[0].Status != session.StatusCompleted {
		t.Errorf("expected 1 completed session, got: %+v", doneSummaries)
	}
}

func TestCLI_Sync_NoApiKeyWarning(t *testing.T) {
	setupTestEnv(t)

	out, err := runCLI(t, []string{"sync"})
	if err != nil {
		t.Fatalf("sync command failed: %v, output: %s", err, out)
	}
	if !strings.Contains(out, "Warning: CLICKUP_API_KEY is not set") {
		t.Errorf("expected CLICKUP_API_KEY warning in output, got: %s", out)
	}
	if !strings.Contains(out, "Sync completed: 0 items processed.") {
		t.Errorf("expected 'Sync completed: 0 items processed.' in output, got: %s", out)
	}
}

func TestCLI_ContextHelpers(t *testing.T) {
	ctx := context.Background()
	if cfg := ConfigFromContext(ctx); cfg != nil {
		t.Errorf("expected nil config from empty context, got %+v", cfg)
	}

	testCfg := &config.Config{Database: config.DatabaseConfig{Path: ":memory:"}}
	ctxWithCfg := context.WithValue(ctx, configKey, testCfg)
	if cfg := ConfigFromContext(ctxWithCfg); cfg != testCfg {
		t.Errorf("expected testCfg, got %+v", cfg)
	}
}

func TestCLI_Serve_GracefulShutdown(t *testing.T) {
	setupTestEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so serve exits cleanly
	cancel()

	rootCmd.SetArgs([]string{"serve"})
	// ExecuteContext will run with pre-cancelled context
	err := rootCmd.ExecuteContext(ctx)
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Logf("serve returned: %v", err)
	}
}

func TestCLI_Start_WithClickUpAPIKey(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir := filepath.Join(tempHome, "myproject_cu")
	_ = os.MkdirAll(projDir, 0755)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-cu-999" {
			_, _ = w.Write([]byte(`{
				"id": "task-cu-999",
				"name": "ClickUp Real Task Name",
				"description": "Real Description",
				"status": {"status": "open", "type": "open"},
				"list": {"id": "list-999", "name": "List"}
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("CLICKUP_API_KEY", "mock-key")
	t.Setenv("CLICKUP_BASE_URL", server.URL)

	out, err := runCLI(t, []string{"start", "task-cu-999", projDir})
	if err != nil {
		t.Fatalf("start failed: %v, output: %s", err, out)
	}
	if !strings.Contains(out, "ClickUp Real Task Name") {
		t.Errorf("expected 'ClickUp Real Task Name' in output, got: %s", out)
	}
}

func TestCLI_Completeness(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir := filepath.Join(tempHome, "dod-project")
	manifest := "version: 1\nrules:\n  - id: r\n    when: { added: \"{name}.go\" }\n    require:\n      - { id: test, describe: \"Test for {name}\", verify: { exists: \"{name}_test.go\" } }\n"
	if err := os.MkdirAll(filepath.Join(projDir, ".taskflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", projDir, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Skipf("skipping: git init failed: %s", out)
	}

	out, err := runCLI(t, []string{"start", "task-cli-dod", projDir, "--json"})
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

	out, err = runCLI(t, []string{"checkpoint", contract.SessionID, "--summary", "widget"})
	if err != nil {
		t.Fatalf("checkpoint failed: %v, output: %s", err, out)
	}
	if !strings.Contains(out, "○ new: Test for widget (expected: widget_test.go)") {
		t.Errorf("checkpoint output should list the new obligation, got: %s", out)
	}

	out, err = runCLI(t, []string{"completeness", contract.SessionID})
	if err != nil {
		t.Fatalf("completeness failed: %v, output: %s", err, out)
	}
	for _, want := range []string{"open: 1 · satisfied: 0", "○ Test for widget [r/test, required]", "expected: widget_test.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("completeness output missing %q, got: %s", want, out)
		}
	}

	out, err = runCLI(t, []string{"completeness", contract.SessionID, "--json"})
	if err != nil {
		t.Fatalf("completeness --json failed: %v, output: %s", err, out)
	}
	var report session.CompletenessReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || !report.Enabled || len(report.Obligations) != 1 {
		t.Errorf("report = %+v, %v", report, err)
	}
}

func TestCLI_Enforcement(t *testing.T) {
	tempHome := setupTestEnv(t)
	projDir := filepath.Join(tempHome, "gate-project")
	manifest := "version: 1\ngate: block\nrules:\n  - id: r\n    when: { added: \"{name}.go\" }\n    plannable: true\n    require:\n" +
		"      - { id: test, describe: \"Test for {name}\", verify: { exists: \"{name}_test.go\" } }\n" +
		"      - { id: docs, describe: \"Docs for {name}\", verify: { exists: \"docs/{name}.md\" } }\n"
	if err := os.MkdirAll(filepath.Join(projDir, ".taskflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", projDir, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Skipf("skipping: git init failed: %s", out)
	}

	out, err := runCLI(t, []string{"start", "task-cli-gate", projDir})
	if err != nil || !strings.Contains(out, "Completeness: .taskflow/completeness.yaml (gate: block)") {
		t.Fatalf("start = %v, output: %s", err, out)
	}
	out, _ = runCLI(t, []string{"list", "--json"})
	var sessions []session.SessionSummary
	if err := json.Unmarshal([]byte(out), &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("list = %v\n%s", err, out)
	}
	sid := sessions[0].SessionID

	if _, err := runCLI(t, []string{"plan", sid, "--var", "name=widget"}); err == nil || !strings.Contains(err.Error(), "--var needs --rule") {
		t.Errorf("plan --var without --rule = %v", err)
	}
	if _, err := runCLI(t, []string{"plan", sid, "--rule", "r", "--var", "widget"}); err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Errorf("plan with a malformed --var = %v", err)
	}
	out, err = runCLI(t, []string{"plan", sid, "--rule", "r", "--var", "name=widget"})
	if err != nil || !strings.Contains(out, "✓ Planned 3 obligation(s)") || !strings.Contains(out, "expected: widget_test.go") {
		t.Fatalf("plan = %v, output: %s", err, out)
	}

	if _, err := runCLI(t, []string{"stop", sid}); !errors.Is(err, session.ErrIncompleteSession) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("blocked stop = %v", err)
	}

	out, err = runCLI(t, []string{"completeness", sid, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var report session.CompletenessReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	var docsID string
	for _, o := range report.Obligations {
		if o.Requirement == "docs" {
			docsID = o.ObligationID
		}
	}
	if _, err := runCLI(t, []string{"waive", sid, docsID}); err == nil {
		t.Error("waive without --rationale should fail")
	}
	out, err = runCLI(t, []string{"waive", sid, docsID, "--rationale", "Internal helper"})
	if err != nil || !strings.Contains(out, "✓ Waived: Docs for widget") || !strings.Contains(out, "waived: 1") {
		t.Fatalf("waive = %v, output: %s", err, out)
	}

	out, err = runCLI(t, []string{"stop", sid, "--force"})
	if err != nil || !strings.Contains(out, "Completion forced with 2 required obligation(s) open") {
		t.Fatalf("stop --force = %v, output: %s", err, out)
	}
}
