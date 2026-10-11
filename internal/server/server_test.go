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

	srv := New(st, cfg, nil)
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

	srv := New(st, &config.Config{}, nil)

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
	srv := New(st, &config.Config{}, nil)

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
