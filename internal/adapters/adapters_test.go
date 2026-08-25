package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/b1codes/taskflow/internal/clickup"
)

func TestGitCapturer_Capture(t *testing.T) {
	t.Parallel()
	g := NewGitCapturer()

	tempDir := t.TempDir()

	// 1. Non-git dir -> returns nil, nil
	ctx, err := g.Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture on non-git dir failed: %v", err)
	}
	if ctx != nil {
		t.Errorf("expected nil context for non-git dir, got %+v", ctx)
	}

	// 2. Git repo with a commit
	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git init failed")
	}
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	file1 := filepath.Join(tempDir, "file1.txt")
	_ = os.WriteFile(file1, []byte("hello"), 0644)
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "init").Run()

	ctx, err = g.Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if ctx == nil {
		t.Fatal("expected non-nil GitContext")
	}
	if ctx.SHA == "" {
		t.Errorf("expected non-empty SHA")
	}
}

func TestClickUpAdapter(t *testing.T) {
	t.Parallel()

	if NewClickUpAdapter(nil) != nil {
		t.Errorf("expected NewClickUpAdapter(nil) to return nil")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-adapter-1" {
			_, _ = w.Write([]byte(`{
				"id": "task-adapter-1",
				"name": "Adapter Task",
				"text_content": "Test Description",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "list-adapter-1", "name": "Adapter List"}
			}`))
			return
		}
		if r.URL.Path == "/list/list-adapter-1" {
			_, _ = w.Write([]byte(`{
				"id": "list-adapter-1",
				"name": "Adapter List",
				"statuses": [
					{"status": "Open", "type": "open", "orderindex": 0},
					{"status": "Closed", "type": "closed", "orderindex": 1}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := clickup.New("test-key").WithBaseURL(server.URL)
	adapter := NewClickUpAdapter(client)

	ctx := context.Background()

	// Test GetTask
	taskInfo, err := adapter.GetTask(ctx, "task-adapter-1")
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskInfo.ID != "task-adapter-1" {
		t.Errorf("expected ID task-adapter-1, got %s", taskInfo.ID)
	}
	if taskInfo.Name != "Adapter Task" {
		t.Errorf("expected Name 'Adapter Task', got %s", taskInfo.Name)
	}
	if taskInfo.Description != "Test Description" {
		t.Errorf("expected Description 'Test Description', got %s", taskInfo.Description)
	}
	if taskInfo.ListID != "list-adapter-1" {
		t.Errorf("expected ListID 'list-adapter-1', got %s", taskInfo.ListID)
	}

	// Test GetListStatuses
	statuses, err := adapter.GetListStatuses(ctx, "list-adapter-1")
	if err != nil {
		t.Fatalf("GetListStatuses failed: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].Status != "Open" || statuses[0].Type != "open" {
		t.Errorf("status 0 mismatch: %+v", statuses[0])
	}
	if statuses[1].Status != "Closed" || statuses[1].Type != "closed" {
		t.Errorf("status 1 mismatch: %+v", statuses[1])
	}

	// Test error path
	_, err = adapter.GetTask(ctx, "nonexistent-task")
	if err == nil {
		t.Errorf("expected error for nonexistent task, got nil")
	}

	_, err = adapter.GetListStatuses(ctx, "nonexistent-list")
	if err == nil {
		t.Errorf("expected error for nonexistent list, got nil")
	}
}
