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

// newStatusUpdateServer serves one task on a list with the given statuses and
// records the status name of every PUT /task/{id}.
func newStatusUpdateServer(t *testing.T, taskID, statusesJSON string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu gosync.Mutex
	var updates []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/task/"+taskID:
			_, _ = w.Write([]byte(`{"id": "` + taskID + `", "name": "Status Task", "list": {"id": "list-` + taskID + `"}}`))
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

func TestWorker_UpdateStatus_ResolvesRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		payload   SyncPayload
		statusMap map[string]string
		want      string
	}{
		{"start", SyncPayload{Role: "start"}, nil, "in progress"},
		{"review", SyncPayload{Role: "review"}, nil, "in review"},
		{"complete", SyncPayload{Role: "complete"}, nil, "complete"},
		{"archive", SyncPayload{Role: "archive"}, nil, "cancelled"},
		{"legacy active payload maps to start", SyncPayload{StatusType: "active"}, nil, "in progress"},
		// Previously resolved to the list's closed status, i.e. "cancelled".
		{"legacy closed payload maps to complete", SyncPayload{StatusType: "closed"}, nil, "complete"},
		{"status_map override", SyncPayload{Role: "start"}, map[string]string{"start": "testing/qa"}, "testing/qa"},
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
			server, updates := newStatusUpdateServer(t, taskID, workerTestStatuses)

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

func TestWorker_UpdateStatus_ReviewWithoutReviewStatus_LeavesTaskUnchanged(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	enqueueStatusOp(t, st, "task-no-review", SyncPayload{Role: "review"}, 5)
	server, updates := newStatusUpdateServer(t, "task-no-review", `[
		{"status": "to do", "type": "open", "orderindex": 0},
		{"status": "doing", "type": "custom", "orderindex": 1},
		{"status": "done", "type": "done", "orderindex": 2}
	]`)

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

func TestWorker_UpdateStatus_Failures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		payload  SyncPayload
		statuses string
		wantErr  string
	}{
		{
			name:     "no matching status",
			payload:  SyncPayload{Role: "archive"},
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "complete", "type": "done", "orderindex": 1}]`,
			wantErr:  "no matching status found",
		},
		{
			name:     "unknown role",
			payload:  SyncPayload{Role: "active"},
			statuses: workerTestStatuses,
			wantErr:  "unknown status role",
		},
		{
			name:     "unmapped legacy status type",
			payload:  SyncPayload{StatusType: "unsupported_type"},
			statuses: workerTestStatuses,
			wantErr:  "unknown status role",
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
			server, updates := newStatusUpdateServer(t, taskID, tt.statuses)

			client := clickup.New("my-key").WithBaseURL(server.URL)
			worker := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1})

			count, _ := worker.DrainOnce(ctx)
			if count != 0 {
				t.Errorf("expected 0 ops successfully processed, got %d", count)
			}
			if got := updates(); len(got) != 0 {
				t.Errorf("expected no status update, got %v", got)
			}

			st.SetRetryBackoffBase(1 * time.Millisecond)
			time.Sleep(5 * time.Millisecond)
			ops, err := st.ListRetryable(ctx)
			if err != nil {
				t.Fatalf("ListRetryable failed: %v", err)
			}
			if len(ops) != 1 {
				t.Fatalf("expected 1 retryable failed op, got %d", len(ops))
			}
			if !strings.Contains(ops[0].ErrorMsg, tt.wantErr) {
				t.Errorf("expected op ErrorMsg to contain %q, got: %q", tt.wantErr, ops[0].ErrorMsg)
			}
		})
	}
}
