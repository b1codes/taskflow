package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

func TestWorker_DrainOnce(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Insert parent session first to satisfy foreign keys
	parentSess := &session.Session{
		ID:          "sess-1",
		TaskID:      "task-123",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadVal := SyncPayload{
		CommentText: "hello clickup",
	}
	payloadBytes, _ := json.Marshal(payloadVal)

	// Enqueue a sync operation
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-123",
		Payload:    string(payloadBytes),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	// Mock server for clickup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	syncCfg := &config.SyncConfig{
		MaxRetries:    5,
		RateLimitMS:   1,
		DrainTimeoutS: 5,
	}

	worker := NewWorker(st, client, syncCfg)
	count, err := worker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed item, got %d", count)
	}

	pendingCount, err := st.CountPendingByTaskID(ctx, "task-123")
	if err != nil {
		t.Fatalf("CountPendingByTaskID failed: %v", err)
	}
	if pendingCount != 0 {
		t.Errorf("expected 0 pending items, got %d", pendingCount)
	}
}
