package internal_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/adapters"
	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/b1codes/taskflow/internal/sync"
)

type gitCapturerMock struct{}

func (g *gitCapturerMock) Capture(projectPath string) (*session.GitContext, error) {
	return &session.GitContext{
		Branch:     "main",
		SHA:        "abcdef123456",
		DirtyFiles: []string{"main.go"},
	}, nil
}

func TestTaskflow_FullLifecycleIntegration(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	var getTaskCalled, postCommentCalled, updateStatusCalled int
	var statusesSet []string
	clickupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/task/task-123" {
			getTaskCalled++
			_, _ = w.Write([]byte(`{
				"id": "task-123",
				"name": "ClickUp Task Title",
				"text_content": "Description",
				"status": {"status": "in progress", "type": "custom"},
				"list": {"id": "list-1", "name": "List Name"}
			}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/list/list-1" {
			_, _ = w.Write([]byte(`{
				"id": "list-1",
				"name": "List Name",
				"statuses": [
					{"status": "Open", "type": "open", "orderindex": 0},
					{"status": "In Progress", "type": "custom", "orderindex": 1},
					{"status": "Complete", "type": "done", "orderindex": 2},
					{"status": "Closed", "type": "closed", "orderindex": 3}
				]
			}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/task/task-123/comment" {
			postCommentCalled++
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == "PUT" && r.URL.Path == "/task/task-123" {
			updateStatusCalled++
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			statusesSet = append(statusesSet, body.Status)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer clickupServer.Close()

	client := clickup.New("test-key").WithBaseURL(clickupServer.URL)
	adapter := adapters.NewClickUpAdapter(client)

	tempDir, err := os.MkdirTemp("", "taskflow-integration-*")
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	service := session.NewService(st, &gitCapturerMock{}, adapter)
	ctx := context.Background()

	// 1. Start session
	startRes, err := service.Start(ctx, "task-123", tempDir, true)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	sessionID := startRes.Session.ID

	if startRes.Session.Status != session.StatusActive {
		t.Errorf("expected session status to be active, got %s", startRes.Session.Status)
	}

	// 2. Create checkpoint
	cp, err := service.Checkpoint(ctx, sessionID, "first commit checkpoint", []string{"main.go"}, true)
	if err != nil {
		t.Fatalf("Checkpoint failed: %v", err)
	}
	if cp.Summary != "first commit checkpoint" {
		t.Errorf("expected checkpoint summary to match, got %s", cp.Summary)
	}

	// 3. Log snag
	snagRes, err := service.LogSnag(ctx, sessionID, "build failure error", "build", "")
	if err != nil {
		t.Fatalf("LogSnag failed: %v", err)
	}
	if snagRes.Snag.Category != "build" {
		t.Errorf("expected snag category to be build, got %s", snagRes.Snag.Category)
	}

	// 4. Stop session
	err = service.Stop(ctx, sessionID, "completed successfully", "COMPLETED", true)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	stoppedSess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("failed to query stopped session: %v", err)
	}
	if stoppedSess.Status != session.StatusCompleted {
		t.Errorf("expected stopped session status to be completed, got %s", stoppedSess.Status)
	}

	// 5. Drain the sync queue via sync Worker
	syncCfg := &config.SyncConfig{
		MaxRetries:  5,
		RateLimitMS: 1,
	}
	worker := sync.NewWorker(st, client, syncCfg)
	count, err := worker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("worker DrainOnce failed: %v", err)
	}
	if count < 4 {
		t.Errorf("expected at least 4 synced operations, got %d", count)
	}

	if getTaskCalled < 1 {
		t.Errorf("expected GetTask to be called, got %d", getTaskCalled)
	}
	if postCommentCalled < 3 {
		t.Errorf("expected at least 3 PostComment calls, got %d", postCommentCalled)
	}
	if updateStatusCalled < 2 {
		t.Errorf("expected at least 2 UpdateTaskStatus calls, got %d", updateStatusCalled)
	}
	// Start moves the task to the first custom status; completing moves it to
	// the done status, never the closed one.
	if len(statusesSet) != 2 || statusesSet[0] != "In Progress" || statusesSet[1] != "Complete" {
		t.Errorf("expected statuses [In Progress Complete], got %v", statusesSet)
	}
}

// TestSyncWorker_AutoDrain verifies that Worker.Run (the ticker-driven
// background loop started by `taskflow serve`) automatically drains enqueued
// sync ops to ClickUp without requiring a manual DrainOnce call.
func TestSyncWorker_AutoDrain(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	var commentPosted atomic.Int32
	var statusUpdated atomic.Int32
	clickupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/task/task-auto" {
			_, _ = w.Write([]byte(`{
				"id": "task-auto",
				"name": "Auto Sync Task",
				"text_content": "Description",
				"status": {"status": "in progress", "type": "custom"},
				"list": {"id": "list-1", "name": "List Name"}
			}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/list/list-1" {
			_, _ = w.Write([]byte(`{
				"id": "list-1",
				"name": "List Name",
				"statuses": [
					{"status": "Open", "type": "open", "orderindex": 0},
					{"status": "In Progress", "type": "custom", "orderindex": 1},
					{"status": "Complete", "type": "done", "orderindex": 2},
					{"status": "Closed", "type": "closed", "orderindex": 3}
				]
			}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/task/task-auto/comment" {
			commentPosted.Add(1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == "PUT" && r.URL.Path == "/task/task-auto" {
			statusUpdated.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer clickupServer.Close()

	client := clickup.New("test-key").WithBaseURL(clickupServer.URL)
	adapter := adapters.NewClickUpAdapter(client)

	tempDir, err := os.MkdirTemp("", "taskflow-autosync-*")
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	service := session.NewService(st, &gitCapturerMock{}, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start the sync worker in the background — this is what `taskflow serve`
	// now does alongside the MCP server.
	syncCfg := &config.SyncConfig{
		MaxRetries:  5,
		RateLimitMS: 1,
	}
	worker := sync.NewWorker(st, client, syncCfg)
	go func() {
		_ = worker.Run(ctx)
	}()

	// Enqueue via service: start a session and create a checkpoint.
	// These calls enqueue sync ops (UPDATE_STATUS, POST_COMMENT) into sync_queue.
	startRes, err := service.Start(ctx, "task-auto", tempDir, false)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	sessionID := startRes.Session.ID

	_, err = service.Checkpoint(ctx, sessionID, "auto-synced checkpoint", nil, false)
	if err != nil {
		t.Fatalf("Checkpoint failed: %v", err)
	}

	// Wait for the worker to automatically drain the enqueued ops.
	// The worker polls every 2s, so we give it up to 5s.
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for sync worker to auto-drain; commentPosted=%d, statusUpdated=%d",
				commentPosted.Load(), statusUpdated.Load())
		case <-ticker.C:
			// Start enqueues UPDATE_STATUS, Checkpoint enqueues POST_COMMENT.
			// We need at least 1 comment posted to prove auto-drain works.
			if commentPosted.Load() >= 1 {
				return
			}
		}
	}
}
