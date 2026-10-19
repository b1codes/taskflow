package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	gosync "sync"
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
	st.SetRetryBackoffBase(10 * time.Millisecond)

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
	st.SetRetryBackoffBase(10 * time.Millisecond)

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
	if _, err := st.MarkInFlight(ctx, opID); err != nil {
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

func TestWorker_RateLimit429_DoesNotConsumeRetry(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	parentSess := &session.Session{
		ID:          "sess-429-worker",
		TaskID:      "task-429-worker",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "test 429 op"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-429-worker",
		Payload:    string(payloadBytes),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := attempts.Add(1)
		if att == 1 {
			// First attempt returns 429 Too Many Requests with Retry-After header
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
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

	// Pass 1: returns 429
	count, _ := worker.DrainOnce(ctx)
	if count != 0 {
		t.Errorf("expected 0 processed on 429, got %d", count)
	}

	// Verify that the op was requeued as PENDING with retries STILL AT 0 (retry not consumed)
	pending, err := st.DequeuePending(ctx, 10)
	if err != nil {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending item after 429 requeue, got %d", len(pending))
	}
	if pending[0].Retries != 0 {
		t.Errorf("expected retries to remain 0 after 429 rate limit, got %d", pending[0].Retries)
	}

	// Re-enqueue op for pass 2 (since DequeuePending fetched it)
	// Now let worker drain it to completion
	pass2Worker := NewWorker(st, client, syncCfg)
	count, err = pass2Worker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce pass 2 failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed item on pass 2, got %d", count)
	}
}

// workerTestStatuses mirrors a real ClickUp list. ClickUp status types are
// only open/custom/done/closed.
const workerTestStatuses = `[
	{"status": "to do", "type": "open", "orderindex": 0},
	{"status": "in progress", "type": "custom", "orderindex": 1},
	{"status": "in review", "type": "custom", "orderindex": 2},
	{"status": "testing/qa", "type": "custom", "orderindex": 3},
	{"status": "complete", "type": "done", "orderindex": 4},
	{"status": "cancelled", "type": "closed", "orderindex": 5}
]`

// enqueueStatusOp creates the parent session and queues an UPDATE_STATUS op.
func enqueueStatusOp(t *testing.T, st store.Store, taskID string, payload SyncPayload, maxRetries int) {
	t.Helper()
	ctx := context.Background()
	sess := &session.Session{
		ID:          "sess-" + taskID,
		TaskID:      taskID,
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	payloadBytes, _ := json.Marshal(payload)
	op := session.SyncOp{
		Operation:  "UPDATE_STATUS",
		TaskID:     taskID,
		Payload:    string(payloadBytes),
		MaxRetries: maxRetries,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}
}

// newStatusUpdateServer serves one task, currently in the given status JSON
// (e.g. {"status": "to do", "type": "open"}), on a list with the given
// statuses, and records the status name of every PUT /task/{id}. getTaskCalls
// counts task fetches and may be nil.
func newStatusUpdateServer(t *testing.T, taskID, taskStatusJSON, statusesJSON string, getTaskCalls *atomic.Int32) (*httptest.Server, func() []string) {
	t.Helper()
	var mu gosync.Mutex
	var updates []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/task/"+taskID:
			if getTaskCalls != nil {
				getTaskCalls.Add(1)
			}
			_, _ = w.Write([]byte(`{"id": "` + taskID + `", "name": "Status Task", "status": ` + taskStatusJSON + `, "list": {"id": "list-` + taskID + `"}}`))
		case r.Method == "GET" && r.URL.Path == "/list/list-"+taskID:
			_, _ = w.Write([]byte(`{"id": "list-` + taskID + `", "statuses": ` + statusesJSON + `}`))
		case r.Method == "PUT" && r.URL.Path == "/task/"+taskID:
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			updates = append(updates, body.Status)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), updates...)
	}
}

const openTaskStatus = `{"status": "to do", "type": "open"}`

func TestWorker_UpdateStatus_ResolvesRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		payload   SyncPayload
		statusMap config.StatusMapConfig
		// sessionStatus is the task's latest local session status; empty
		// leaves it ACTIVE.
		sessionStatus session.Status
		want          string
	}{
		{"start", SyncPayload{Role: "start"}, config.StatusMapConfig{}, "", "in progress"},
		{"complete", SyncPayload{Role: "complete"}, config.StatusMapConfig{}, "", "complete"},
		{"archive", SyncPayload{Role: "archive"}, config.StatusMapConfig{}, "", "cancelled"},
		{"legacy active payload maps to start", SyncPayload{StatusType: "active"}, config.StatusMapConfig{}, "", "in progress"},
		// The old Stop sent "closed" for both completing and archiving; the
		// task's latest session tells which.
		{"legacy closed payload from a completed session maps to complete", SyncPayload{StatusType: "closed"}, config.StatusMapConfig{}, session.StatusCompleted, "complete"},
		{"legacy closed payload from an archived session maps to archive", SyncPayload{StatusType: "closed"}, config.StatusMapConfig{}, session.StatusArchived, "cancelled"},
		{"status_map override", SyncPayload{Role: "start"}, config.StatusMapConfig{Start: "testing/qa"}, "", "testing/qa"},
		{"status_map reopen override", SyncPayload{Role: "reopen"}, config.StatusMapConfig{Start: "in progress", Reopen: "testing/qa"}, "", "testing/qa"},
		{"review", SyncPayload{Role: "review"}, config.StatusMapConfig{}, "", "in review"},
		{"status_map review override", SyncPayload{Role: "review"}, config.StatusMapConfig{Review: "testing/qa"}, "", "testing/qa"},
		{"explicit target_status override", SyncPayload{Role: "complete", TargetStatus: "in review"}, config.StatusMapConfig{}, "", "in review"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.New(":memory:")
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}
			defer st.Close()

			taskID := fmt.Sprintf("task-role-%d", i)
			enqueueStatusOp(t, st, taskID, tt.payload, 5)
			if tt.sessionStatus != "" {
				if err := st.UpdateSessionStatus(context.Background(), "sess-"+taskID, tt.sessionStatus); err != nil {
					t.Fatalf("UpdateSessionStatus failed: %v", err)
				}
			}
			server, updates := newStatusUpdateServer(t, taskID, openTaskStatus, workerTestStatuses, nil)

			client := clickup.New("my-key").WithBaseURL(server.URL)
			worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 5, RateLimitMS: 1}).WithStatusMap(tt.statusMap)

			count, err := worker.DrainOnce(context.Background())
			if err != nil {
				t.Fatalf("DrainOnce failed: %v", err)
			}
			if count != 1 {
				t.Errorf("expected 1 op processed, got %d", count)
			}
			if got := updates(); len(got) != 1 || got[0] != tt.want {
				t.Errorf("expected one status update to %q, got %v", tt.want, got)
			}
		})
	}
}

// TestWorker_UpdateStatus_TaskPastRole_LeavesTaskUnchanged covers re-archiving
// a completed session: the task is already done, so archive must not move it
// to "cancelled", and the skipped op must not be retried.
func TestWorker_UpdateStatus_TaskPastRole_LeavesTaskUnchanged(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	enqueueStatusOp(t, st, "task-done", SyncPayload{Role: "archive"}, 5)
	server, updates := newStatusUpdateServer(t, "task-done", `{"status": "complete", "type": "done"}`, workerTestStatuses, nil)

	client := clickup.New("my-key").WithBaseURL(server.URL)
	worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 5, RateLimitMS: 1})

	count, err := worker.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected skipped op to count as processed, got %d", count)
	}
	if got := updates(); len(got) != 0 {
		t.Errorf("expected no status update, got %v", got)
	}

	st.SetRetryBackoffBase(1 * time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if ops, _ := st.ListRetryable(ctx); len(ops) != 0 {
		t.Errorf("expected skipped op not to be retried, got %d retryable ops", len(ops))
	}
}

// TestWorker_UpdateStatus_PermanentFailures checks that errors which would
// recur on every attempt fail the op at once instead of consuming max_retries.
func TestWorker_UpdateStatus_PermanentFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		payload  SyncPayload
		statuses string
	}{
		{
			name:     "no matching status",
			payload:  SyncPayload{Role: "complete"},
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "doing", "type": "custom", "orderindex": 1}]`,
		},
		{
			name:     "unknown role",
			payload:  SyncPayload{Role: "deploy"},
			statuses: workerTestStatuses,
		},
		{
			name:     "unmapped legacy status type",
			payload:  SyncPayload{StatusType: "unsupported_type"},
			statuses: workerTestStatuses,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.New(":memory:")
			if err != nil {
				t.Fatalf("failed to create store: %v", err)
			}
			defer st.Close()
			ctx := context.Background()

			taskID := fmt.Sprintf("task-fail-%d", i)
			enqueueStatusOp(t, st, taskID, tt.payload, 3)
			var getTaskCalls atomic.Int32
			server, updates := newStatusUpdateServer(t, taskID, openTaskStatus, tt.statuses, &getTaskCalls)

			client := clickup.New("my-key").WithBaseURL(server.URL)
			worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1})

			if count, _ := worker.DrainOnce(ctx); count != 0 {
				t.Errorf("expected 0 ops successfully processed, got %d", count)
			}
			callsAfterFirstDrain := getTaskCalls.Load()

			st.SetRetryBackoffBase(1 * time.Millisecond)
			time.Sleep(5 * time.Millisecond)
			if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
				t.Fatalf("expected no retryable ops, got %d (err: %v)", len(ops), err)
			}

			_, _ = worker.DrainOnce(ctx)
			if got := getTaskCalls.Load(); got != callsAfterFirstDrain {
				t.Errorf("expected the failed op not to be attempted again, task fetches went %d -> %d", callsAfterFirstDrain, got)
			}
			if got := updates(); len(got) != 0 {
				t.Errorf("expected no status update, got %v", got)
			}
		})
	}
}

// TestWorker_BadRequest_FailsPermanently checks that a 400 from ClickUp fails
// the op at once: the request itself is rejected, so retrying can't help.
func TestWorker_BadRequest_FailsPermanently(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	sess := &session.Session{
		ID:          "sess-bad-request",
		TaskID:      "task-bad-request",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	payloadBytes, _ := json.Marshal(SyncPayload{CommentText: "rejected"})
	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-bad-request",
		Payload:    string(payloadBytes),
		MaxRetries: 3,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"err": "invalid comment"}`))
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1})

	if count, _ := worker.DrainOnce(ctx); count != 0 {
		t.Errorf("expected 0 ops successfully processed, got %d", count)
	}

	st.SetRetryBackoffBase(1 * time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("expected no retryable ops, got %d (err: %v)", len(ops), err)
	}

	_, _ = worker.DrainOnce(ctx)
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", got)
	}
}

// TestWorker_LookupBadRequest_IsRetried checks that a 400 from a lookup made
// while applying a status role (here, fetching the task) consumes a retry
// instead of failing the op: only a 400 to the op's own write is permanent.
func TestWorker_LookupBadRequest_IsRetried(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	enqueueStatusOp(t, st, "task-lookup-400", SyncPayload{Role: "start"}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"err": "transient"}`))
	}))
	defer server.Close()

	client := clickup.New("my-key").WithBaseURL(server.URL)
	worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1})
	if count, _ := worker.DrainOnce(ctx); count != 0 {
		t.Errorf("expected 0 ops successfully processed, got %d", count)
	}

	st.SetRetryBackoffBase(1 * time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	ops, err := st.ListRetryable(ctx)
	if err != nil {
		t.Fatalf("ListRetryable failed: %v", err)
	}
	if len(ops) != 1 || ops[0].Retries != 1 {
		t.Fatalf("expected the op to be retryable after one attempt, got %+v", ops)
	}
}

// pruneRecorder records the PruneSyncQueue calls and claims made on a store.
type pruneRecorder struct {
	store.Store
	mu     gosync.Mutex
	events []string
	ages   []time.Duration
}

func (r *pruneRecorder) PruneSyncQueue(ctx context.Context, olderThan time.Duration) (int64, error) {
	r.mu.Lock()
	r.events, r.ages = append(r.events, "prune"), append(r.ages, olderThan)
	r.mu.Unlock()
	return r.Store.PruneSyncQueue(ctx, olderThan)
}

func (r *pruneRecorder) MarkInFlight(ctx context.Context, id int64) (*session.SyncOp, error) {
	r.mu.Lock()
	r.events = append(r.events, "claim")
	r.mu.Unlock()
	return r.Store.MarkInFlight(ctx, id)
}

func (r *pruneRecorder) snapshot() ([]string, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...), append([]time.Duration(nil), r.ages...)
}

func newPruneRecorder(t *testing.T) *pruneRecorder {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &pruneRecorder{Store: st}
}

func TestWorker_Prune(t *testing.T) {
	t.Parallel()
	rec := newPruneRecorder(t)
	ctx := context.Background()

	// Retention 0 keeps finished ops forever: the store isn't asked.
	if n, err := NewWorker(rec, nil, &config.SyncConfig{}).Prune(ctx); err != nil || n != 0 {
		t.Errorf("Prune with no retention = %d, %v; want 0", n, err)
	}
	if _, err := NewWorker(rec, nil, &config.SyncConfig{RetentionDays: 30}).Prune(ctx); err != nil {
		t.Errorf("Prune failed: %v", err)
	}
	if _, ages := rec.snapshot(); len(ages) != 1 || ages[0] != 30*24*time.Hour {
		t.Errorf("prunes = %v, want one of 30 days", ages)
	}
}

func TestWorker_Run_NilClient_PrunesPeriodically(t *testing.T) {
	t.Parallel()
	rec := newPruneRecorder(t)
	w := NewWorker(rec, nil, &config.SyncConfig{RetentionDays: 30})
	w.pruneEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if events, _ := rec.snapshot(); len(events) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run without a client stopped pruning after startup")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestWorker_Run_PrunesOnlyOnceIdle(t *testing.T) {
	t.Parallel()
	rec := newPruneRecorder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rec.EnqueueSync(ctx, session.SyncOp{Operation: "POST_COMMENT", TaskID: "task-1", Payload: `{"comment_text":"hi"}`, MaxRetries: 5, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1"}`))
	}))
	defer server.Close()
	w := NewWorker(rec, clickup.New("my-key").WithBaseURL(server.URL), &config.SyncConfig{RetentionDays: 30})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if events, _ := rec.snapshot(); len(events) >= 2 {
			if events[0] != "claim" || events[1] != "prune" {
				t.Errorf("events = %v; want the pending op claimed before the first prune", events)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never pruned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
