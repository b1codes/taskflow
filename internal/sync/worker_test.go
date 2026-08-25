package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

func TestWorker_DrainOnce(t *testing.T) {
	t.Parallel()
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

func TestWorker_Run_RetriesFailedOps(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	parentSess := &session.Session{
		ID:          "sess-retry",
		TaskID:      "task-retry",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "retry me"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-retry",
		Payload:    string(payloadBytes),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			// First attempt fails with 500
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Subsequent attempts succeed
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	syncCfg := &config.SyncConfig{
		MaxRetries:  5,
		RateLimitMS: 1,
	}

	worker := NewWorker(st, client, syncCfg)
	go func() {
		_ = worker.Run(ctx)
	}()

	// Wait until the op is retried and reaches DONE
	deadline := time.After(6 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for op to be retried; attempts=%d", attempts.Load())
		case <-ticker.C:
			if attempts.Load() >= 2 {
				retryable, err := st.ListRetryable(ctx)
				if err != nil {
					t.Fatalf("ListRetryable failed: %v", err)
				}
				pending, err := st.CountPendingByTaskID(ctx, "task-retry")
				if err != nil {
					t.Fatalf("CountPendingByTaskID failed: %v", err)
				}
				if len(retryable) == 0 && pending == 0 {
					// Succeeded and reached DONE
					return
				}
			}
		}
	}
}

func TestWorker_Run_ExceedingMaxRetriesStops(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	parentSess := &session.Session{
		ID:          "sess-max-retries",
		TaskID:      "task-max-retries",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "always fail"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-max-retries",
		Payload:    string(payloadBytes),
		MaxRetries: 2,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	syncCfg := &config.SyncConfig{
		MaxRetries:  2,
		RateLimitMS: 1,
	}

	worker := NewWorker(st, client, syncCfg)
	go func() {
		_ = worker.Run(ctx)
	}()

	// Wait until attempts reach max_retries (2), then verify no further attempts are made
	deadline := time.After(6 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			if attempts.Load() != 2 {
				t.Fatalf("expected exactly 2 attempts before stopping, got %d", attempts.Load())
			}
			return
		case <-ticker.C:
			if attempts.Load() > 2 {
				t.Fatalf("attempts exceeded max_retries: %d", attempts.Load())
			}
		}
	}
}

func TestWorker_Reaper_RecoversStrandedInFlight(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	parentSess := &session.Session{
		ID:          "sess-reap-worker",
		TaskID:      "task-reap-worker",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "stranded in flight"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-reap-worker",
		Payload:    string(payloadBytes),
		MaxRetries: 5,
		CreatedAt:  time.Now().Add(-20 * time.Minute),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	// Simulate crash during processing: op was marked in flight
	if err := st.MarkInFlight(ctx, opID); err != nil {
		t.Fatalf("MarkInFlight failed: %v", err)
	}

	// Reset with 0 duration to simulate staleness
	resetRows, err := st.ResetStaleInFlight(ctx, 0)
	if err != nil {
		t.Fatalf("ResetStaleInFlight failed: %v", err)
	}
	if resetRows != 1 {
		t.Fatalf("expected 1 row reset, got %d", resetRows)
	}

	var commentsPosted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commentsPosted.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	syncCfg := &config.SyncConfig{
		MaxRetries:  5,
		RateLimitMS: 1,
	}

	worker := NewWorker(st, client, syncCfg)
	count, err := worker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed op, got %d", count)
	}
	if commentsPosted.Load() != 1 {
		t.Errorf("expected 1 comment posted, got %d", commentsPosted.Load())
	}
}

func TestWorker_NilClient_LeavesOpsPendingAndDrainsLater(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	parentSess := &session.Session{
		ID:          "sess-nil-client",
		TaskID:      "task-nil-client",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "test op 1"})
	op1 := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-nil-client",
		Payload:    string(payloadBytes),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op1); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	payloadBytes2, _ := json.Marshal(SyncPayload{CommentText: "test op 2"})
	op2 := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-nil-client",
		Payload:    string(payloadBytes2),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op2); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	syncCfg := &config.SyncConfig{
		MaxRetries:  5,
		RateLimitMS: 1,
	}

	// 1. Run with nil client: should report 0 processed and leave ops in PENDING
	nilWorker := NewWorker(st, nil, syncCfg)
	count, err := nilWorker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce with nil client failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed with nil client, got %d", count)
	}

	pendingCount, err := st.CountPendingByTaskID(ctx, "task-nil-client")
	if err != nil {
		t.Fatalf("CountPendingByTaskID failed: %v", err)
	}
	if pendingCount != 2 {
		t.Fatalf("expected 2 pending ops retained, got %d", pendingCount)
	}

	// 2. Configure a valid client and server: should drain backlog successfully
	var serverRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := clickup.New("valid-key").WithBaseURL(server.URL)
	configuredWorker := NewWorker(st, client, syncCfg)

	count, err = configuredWorker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce with configured client failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 processed ops, got %d", count)
	}
	if serverRequests.Load() != 2 {
		t.Errorf("expected 2 server requests, got %d", serverRequests.Load())
	}

	pendingCount, err = st.CountPendingByTaskID(ctx, "task-nil-client")
	if err != nil {
		t.Fatalf("CountPendingByTaskID failed: %v", err)
	}
	if pendingCount != 0 {
		t.Errorf("expected 0 pending ops left after drain, got %d", pendingCount)
	}
}

func TestWorker_LogsFailureToStderr(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	parentSess := &session.Session{
		ID:          "sess-log-test",
		TaskID:      "task-log-test",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "test log fail"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-log-test",
		Payload:    string(payloadBytes),
		MaxRetries: 3,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	syncCfg := &config.SyncConfig{
		MaxRetries:  3,
		RateLimitMS: 1,
	}

	// Capture stderr
	oldStderr := os.Stderr
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("failed to create pipe: %v", pipeErr)
	}
	os.Stderr = w

	worker := NewWorker(st, client, syncCfg)
	_, _ = worker.DrainOnce(ctx)

	// Restore stderr
	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	stderrOutput := buf.String()

	if !strings.Contains(stderrOutput, "[sync] op") || !strings.Contains(stderrOutput, "failed") {
		t.Errorf("expected stderr to contain '[sync] op ... failed', got: %q", stderrOutput)
	}
	if !strings.Contains(stderrOutput, "task-log-test") {
		t.Errorf("expected stderr to contain task ID 'task-log-test', got: %q", stderrOutput)
	}
}





