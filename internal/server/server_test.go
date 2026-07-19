package server

import (
	"context"
	"encoding/json"
	"testing"

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

	srv := New(st, cfg)
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
