package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMCPServer_Handlers(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		Git: config.GitConfig{
			AutoContext: false,
		},
	}

	srv := mustNew(t, st, cfg)
	ctx := context.Background()

	// 1. Test tf_start handler
	startReq := mcp.CallToolRequest{}
	startReq.Params.Arguments = map[string]any{
		"task_id":      "task-123",
		"project_path": "/tmp/my-mcp-proj",
	}

	startRes, err := srv.handleStart(ctx, startReq)
	if err != nil {
		t.Fatalf("handleStart failed: %v", err)
	}
	if startRes.IsError {
		t.Fatalf("handleStart returned error: %s", startRes.Content[0].(mcp.TextContent).Text)
	}

	var contract session.AgenticContract
	err = json.Unmarshal([]byte(startRes.Content[0].(mcp.TextContent).Text), &contract)
	if err != nil {
		t.Fatalf("failed to parse contract: %v", err)
	}

	sessionID := contract.SessionID
	if sessionID == "" {
		t.Fatalf("expected session ID to be populated")
	}

	// 2. Test tf_checkpoint handler
	checkpointReq := mcp.CallToolRequest{}
	checkpointReq.Params.Arguments = map[string]any{
		"session_id": sessionID,
		"summary":    "finished mcp setup",
		"files":      []any{"server.go"},
	}

	checkpointRes, err := srv.handleCheckpoint(ctx, checkpointReq)
	if err != nil {
		t.Fatalf("handleCheckpoint failed: %v", err)
	}
	if checkpointRes.IsError {
		t.Fatalf("handleCheckpoint returned error: %s", checkpointRes.Content[0].(mcp.TextContent).Text)
	}

	// 3. Test tf_snag handler
	snagReq := mcp.CallToolRequest{}
	snagReq.Params.Arguments = map[string]any{
		"session_id": sessionID,
		"error":      "compilation error",
		"category":   "build",
	}

	snagRes, err := srv.handleSnag(ctx, snagReq)
	if err != nil {
		t.Fatalf("handleSnag failed: %v", err)
	}
	if snagRes.IsError {
		t.Fatalf("handleSnag returned error: %s", snagRes.Content[0].(mcp.TextContent).Text)
	}

	// 4. Test tf_list handler
	listReq := mcp.CallToolRequest{}
	listReq.Params.Arguments = map[string]any{
		"status": "active",
	}

	listRes, err := srv.handleList(ctx, listReq)
	if err != nil {
		t.Fatalf("handleList failed: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("handleList returned error: %s", listRes.Content[0].(mcp.TextContent).Text)
	}

	// 5. Test tf_stop handler
	stopReq := mcp.CallToolRequest{}
	stopReq.Params.Arguments = map[string]any{
		"session_id": sessionID,
		"summary":    "all done",
		"status":     "completed",
	}

	stopRes, err := srv.handleStop(ctx, stopReq)
	if err != nil {
		t.Fatalf("handleStop failed: %v", err)
	}
	if stopRes.IsError {
		t.Fatalf("handleStop returned error: %s", stopRes.Content[0].(mcp.TextContent).Text)
	}
}

func TestServer_RunReader_ContextCancellation(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	srv := mustNew(t, st, &config.Config{})

	ctx, cancel := context.WithCancel(context.Background())

	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	var outBuf bytes.Buffer

	doneChan := make(chan error, 1)
	go func() {
		doneChan <- srv.RunReader(ctx, inReader, &outBuf)
	}()

	// Cancel context to trigger shutdown
	cancel()

	select {
	case err := <-doneChan:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("RunReader returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunReader did not terminate upon context cancellation")
	}
}

func callTool(t *testing.T, name string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s failed: %v", name, err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s returned error: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s returned invalid JSON: %v\n%s", name, err, text)
	}
	return out
}

func TestMCPServer_Completeness(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	srv := mustNew(t, st, &config.Config{})

	t.Run("no manifest", func(t *testing.T) {
		contract := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-plain", "project_path": t.TempDir()})
		if _, ok := contract["completeness"]; ok {
			t.Errorf("contract must not have completeness without a manifest")
		}
		sid := contract["session_id"].(string)
		cp := callTool(t, "tf_checkpoint", srv.handleCheckpoint, map[string]any{"session_id": sid, "summary": "s"})
		if _, ok := cp["completeness"]; ok {
			t.Errorf("checkpoint must not have completeness without a manifest")
		}
		report := callTool(t, "tf_completeness", srv.handleCompleteness, map[string]any{"session_id": sid})
		if report["enabled"] != false || report["session_id"] != sid {
			t.Errorf("report = %v", report)
		}
	})

	t.Run("with manifest", func(t *testing.T) {
		dir := t.TempDir()
		if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
			t.Skip("skipping: git init failed")
		}
		manifest := "version: 1\nrules:\n  - id: r\n    when: { added: \"{name}.go\" }\n    require:\n      - { id: test, describe: \"Test for {name}\", verify: { exists: \"{name}_test.go\" } }\n"
		if err := os.MkdirAll(filepath.Join(dir, ".taskflow"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}

		contract := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-dod", "project_path": dir})
		cc, ok := contract["completeness"].(map[string]any)
		if !ok || cc["manifest"] != ".taskflow/completeness.yaml" {
			t.Fatalf("contract completeness = %v", contract["completeness"])
		}
		sid := contract["session_id"].(string)

		if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cp := callTool(t, "tf_checkpoint", srv.handleCheckpoint, map[string]any{"session_id": sid, "summary": "added widget", "files": []any{"widget.go"}})
		for _, k := range []string{"id", "session_id", "summary", "files", "created_at"} {
			if _, ok := cp[k]; !ok {
				t.Errorf("checkpoint field %q must stay at the top level: %v", k, cp)
			}
		}
		comp := cp["completeness"].(map[string]any)
		if summary := comp["summary"].(map[string]any); summary["open"] != float64(1) {
			t.Errorf("summary = %v", summary)
		}
		newlyOpen := comp["newly_open"].([]any)
		if len(newlyOpen) != 1 {
			t.Fatalf("newly_open = %v", newlyOpen)
		}
		item := newlyOpen[0].(map[string]any)
		for k, want := range map[string]any{"describe": "Test for widget", "expected": "widget_test.go", "severity": "required", "rule": "r"} {
			if item[k] != want {
				t.Errorf("newly_open[0].%s = %v, want %v", k, item[k], want)
			}
		}
		if item["obligation_id"] == "" || comp["newly_satisfied"] == nil || comp["still_open"] == nil {
			t.Errorf("checkpoint completeness shape = %v", comp)
		}

		report := callTool(t, "tf_completeness", srv.handleCompleteness, map[string]any{"session_id": sid})
		if report["enabled"] != true || len(report["obligations"].([]any)) != 1 || len(report["rules"].([]any)) != 1 {
			t.Errorf("report = %v", report)
		}
		obl := report["obligations"].([]any)[0].(map[string]any)
		if obl["status"] != "open" || obl["subject"] != "widget.go" {
			t.Errorf("report obligation = %v", obl)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"session_id": "nope"}
		res, err := srv.handleCompleteness(context.Background(), req)
		if err != nil || !res.IsError {
			t.Errorf("expected a tool error for an unknown session, got %v, %v", res, err)
		}
	})
}

// callToolError calls a handler expecting a tool error, and returns its text.
func callToolError(t *testing.T, name string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s failed: %v", name, err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !res.IsError {
		t.Fatalf("%s should have returned a tool error, got: %s", name, text)
	}
	return text
}

func TestMCPServer_Enforcement(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping: git init failed")
	}
	manifest := `version: 1
gate: block
rules:
  - id: r
    when: { added: "{name}.go" }
    plannable: true
    require:
      - { id: test, describe: "Test for {name}", verify: { exists: "{name}_test.go" }, attest: "Name the test cases covering {name}" }
      - { id: docs, describe: "Docs for {name}", verify: { exists: "docs/{name}.md" } }
`
	if err := os.MkdirAll(filepath.Join(dir, ".taskflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	srv := mustNew(t, st, &config.Config{Completeness: config.CompletenessConfig{Gate: "report"}})

	contract := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-gate", "project_path": dir})
	sid := contract["session_id"].(string)
	cc := contract["completeness"].(map[string]any)
	for _, k := range []string{"manifest", "gate", "stacks", "rules", "summary", "open", "protocol"} {
		if _, ok := cc[k]; !ok {
			t.Errorf("contract completeness lacks %q: %v", k, cc)
		}
	}
	if cc["gate"] != "block" {
		t.Errorf("the manifest's gate should override config: %v", cc["gate"])
	}

	// tf_plan: items are parsed from objects; non-string vars are rejected.
	if text := callToolError(t, "tf_plan", srv.handlePlan, map[string]any{"session_id": sid, "items": []any{map[string]any{"rule_id": "r", "vars": map[string]any{"name": 3}}}}); !strings.Contains(text, "items must be") {
		t.Errorf("tf_plan with a numeric var = %q", text)
	}
	if text := callToolError(t, "tf_plan", srv.handlePlan, map[string]any{"session_id": sid, "items": []any{map[string]any{"rule_id": "nope"}}}); !strings.Contains(text, `unknown rule "nope"`) {
		t.Errorf("tf_plan with an unknown rule = %q", text)
	}
	plan := callTool(t, "tf_plan", srv.handlePlan, map[string]any{"session_id": sid, "items": []any{map[string]any{"rule_id": "r", "vars": map[string]any{"name": "widget"}}}})
	planned := plan["planned"].([]any)
	if plan["plan_state"] != "planned" || len(planned) != 3 {
		t.Fatalf("tf_plan = %v", plan)
	}
	ids := map[string]string{}
	for _, p := range planned {
		p := p.(map[string]any)
		ids[p["requirement"].(string)] = p["obligation_id"].(string)
	}

	// tf_stop is blocked, and has no force.
	if text := callToolError(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid, "status": "completed", "force": true}); !strings.Contains(text, "tf_waive") || !strings.Contains(text, ids["docs"]) {
		t.Errorf("blocked tf_stop = %q", text)
	}

	// tf_waive and tf_attest resolve what's open.
	if text := callToolError(t, "tf_waive", srv.handleWaive, map[string]any{"session_id": sid, "obligation_id": "nope", "rationale": "x"}); !strings.Contains(text, ids["docs"]) {
		t.Errorf("tf_waive on an unknown obligation should name the open ones: %q", text)
	}
	callToolError(t, "tf_waive", srv.handleWaive, map[string]any{"session_id": sid, "obligation_id": ids["docs"]})
	waived := callTool(t, "tf_waive", srv.handleWaive, map[string]any{"session_id": sid, "obligation_id": ids["docs"], "rationale": "Internal helper; no user docs"})
	if waived["decision_id"] == "" || waived["obligation"].(map[string]any)["status"] != "waived" {
		t.Errorf("tf_waive = %v", waived)
	}
	attested := callTool(t, "tf_attest", srv.handleAttest, map[string]any{"session_id": sid, "obligation_id": ids["test"], "evidence": "widget_test.go: TestWidget"})
	if attested["obligation"].(map[string]any)["status"] != "attested" {
		t.Errorf("tf_attest test = %v", attested)
	}
	// The subject file is checked from files and asks for no attestation:
	// tf_attest can't stand in for creating it, and the gate still blocks.
	if text := callToolError(t, "tf_attest", srv.handleAttest, map[string]any{"session_id": sid, "obligation_id": ids["subject-file"], "evidence": "done in another repo"}); !strings.Contains(text, "asks for no attestation") || !strings.Contains(text, "tf_waive") {
		t.Errorf("tf_attest on a file-checked obligation = %q", text)
	}
	if text := callToolError(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid, "status": "completed"}); !strings.Contains(text, ids["subject-file"]) {
		t.Errorf("tf_stop after a refused attestation = %q", text)
	}
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := callTool(t, "tf_stop", srv.handleStop, map[string]any{"session_id": sid, "status": "completed"})
	if stop["status"] != "ok" || stop["session_id"] != sid || stop["new_status"] != "COMPLETED" {
		t.Errorf("tf_stop = %v", stop)
	}
	if gate := stop["gate"].(map[string]any); gate["mode"] != "block" || gate["outcome"] != "passed" {
		t.Errorf("tf_stop gate = %v", gate)
	}
	if _, ok := stop["completeness"].(map[string]any); !ok {
		t.Errorf("tf_stop should carry the final report: %v", stop)
	}
}

// TestMCPServer_PlanDecisionSurvivesRestart: with the real store, a task
// restarted after a completed session keeps its declined plan, so the new
// session isn't asked to plan again.
func TestMCPServer_PlanDecisionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping: git init failed")
	}
	manifest := `version: 1
rules:
  - id: r
    when: { added: "{name}.go" }
    plannable: true
    plan_hints: [local]
    require:
      - { id: test, describe: "Test for {name}", verify: { exists: "{name}_test.go" } }
`
	if err := os.MkdirAll(filepath.Join(dir, ".taskflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".taskflow", "completeness.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	srv := mustNew(t, st, &config.Config{})

	// Without ClickUp the task is named "Local Task", which the hint matches.
	first := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-plan", "project_path": dir})
	prev := first["session_id"].(string)
	if _, ok := first["completeness"].(map[string]any)["suggested_plan"]; !ok {
		t.Fatalf("first start should suggest a plan: %v", first["completeness"])
	}
	callTool(t, "tf_plan", srv.handlePlan, map[string]any{"session_id": prev, "decline_reason": "Config change only"})
	callTool(t, "tf_stop", srv.handleStop, map[string]any{"session_id": prev, "status": "completed"})

	second := callTool(t, "tf_start", srv.handleStart, map[string]any{"task_id": "task-plan", "project_path": dir})
	sid := second["session_id"].(string)
	if sid == prev {
		t.Fatal("restarting a completed task must create a new session")
	}
	if sp, ok := second["completeness"].(map[string]any)["suggested_plan"]; ok {
		t.Errorf("restart suggested a plan again: %v", sp)
	}
	if sess, err := st.GetSession(context.Background(), sid); err != nil || sess.PlanState != session.PlanDeclined {
		t.Errorf("new session plan state = %+v, %v; want declined", sess, err)
	}
	cp := callTool(t, "tf_checkpoint", srv.handleCheckpoint, map[string]any{"session_id": sid, "summary": "work"})
	if r, ok := cp["completeness"].(map[string]any)["plan_reminder"]; ok {
		t.Errorf("checkpoint plan reminder = %v", r)
	}
}

// mustNew builds a Server, failing the test if cfg is rejected.
func mustNew(t *testing.T, st store.Store, cfg *config.Config) *Server {
	t.Helper()
	srv, err := New(st, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestNew_RejectsInvalidGate(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// A Config not built by config.LoadFrom skips its validation; New must
	// still refuse the typo instead of running with a gate that fails open.
	srv, err := New(st, &config.Config{Completeness: config.CompletenessConfig{Gate: "blok"}}, nil)
	if err == nil || srv != nil || !strings.Contains(err.Error(), `"blok"`) {
		t.Errorf("New with gate blok = %v, %v; want an invalid gate error", srv, err)
	}
}
