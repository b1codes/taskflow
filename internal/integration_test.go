package internal_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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

type clickupAdapter struct {
	client *clickup.Client
}

func (a *clickupAdapter) GetTask(ctx context.Context, taskID string) (*session.ClickUpTaskInfo, error) {
	t, err := a.client.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return &session.ClickUpTaskInfo{
		ID:          t.ID,
		Name:        t.Name,
		Description: t.Description,
		ListID:      t.List.ID,
	}, nil
}

func (a *clickupAdapter) GetListStatuses(ctx context.Context, listID string) ([]session.ClickUpStatusInfo, error) {
	details, err := a.client.GetListDetails(ctx, listID)
	if err != nil {
		return nil, err
	}
	var res []session.ClickUpStatusInfo
	for _, s := range details.Statuses {
		res = append(res, session.ClickUpStatusInfo{
			Status:     s.Status,
			Type:       s.Type,
			OrderIndex: s.OrderIndex,
		})
	}
	return res, nil
}

func TestTaskflow_FullLifecycleIntegration(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	var getTaskCalled, postCommentCalled, updateStatusCalled int
	clickupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/task/task-123" {
			getTaskCalled++
			_, _ = w.Write([]byte(`{
				"id": "task-123",
				"name": "ClickUp Task Title",
				"text_content": "Description",
				"status": {"status": "in progress", "type": "active"},
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
					{"status": "In Progress", "type": "active", "orderindex": 1},
					{"status": "Closed", "type": "closed", "orderindex": 2}
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
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer clickupServer.Close()

	client := clickup.New("test-key").WithBaseURL(clickupServer.URL)
	adapter := &clickupAdapter{client: client}

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
}
