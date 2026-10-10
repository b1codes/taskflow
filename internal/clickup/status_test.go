package clickup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

func TestResolveStatus(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	var getTaskCalls atomic.Int32
	var getListCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-123" {
			getTaskCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id": "task-123",
				"name": "Test Task",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "list-abc", "name": "Sprint 1"}
			}`))
			return
		}
		if r.URL.Path == "/list/list-abc" {
			getListCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id": "list-abc",
				"name": "Sprint 1",
				"statuses": [
					{"status": "open", "type": "open", "orderindex": 0},
					{"status": "in progress", "type": "active", "orderindex": 1},
					{"status": "done", "type": "closed", "orderindex": 2}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)

	// 1. Initial resolution with empty cache (triggers refresh from API for both task and list)
	statusName, err := ResolveStatus(ctx, st, client, "task-123", "closed")
	if err != nil {
		t.Fatalf("ResolveStatus failed: %v", err)
	}
	if statusName != "done" {
		t.Errorf("expected status 'done' for type 'closed', got %q", statusName)
	}
	if getTaskCalls.Load() != 1 {
		t.Errorf("expected 1 getTask call, got %d", getTaskCalls.Load())
	}
	if getListCalls.Load() != 1 {
		t.Errorf("expected 1 getList call, got %d", getListCalls.Load())
	}

	// 2. Subsequent resolution (cache hit for both task -> list mapping and statuses)
	statusActive, err := ResolveStatus(ctx, st, client, "task-123", "active")
	if err != nil {
		t.Fatalf("ResolveStatus cache hit failed: %v", err)
	}
	if statusActive != "in progress" {
		t.Errorf("expected status 'in progress' for type 'active', got %q", statusActive)
	}
	// Verify neither task nor list API was called again
	if getTaskCalls.Load() != 1 {
		t.Errorf("expected still 1 getTask call, got %d", getTaskCalls.Load())
	}
	if getListCalls.Load() != 1 {
		t.Errorf("expected still 1 getList call, got %d", getListCalls.Load())
	}

	// 3. Unknown target type returns ErrNoMatchingStatus
	_, err = ResolveStatus(ctx, st, client, "task-123", "nonexistent_type")
	if err == nil {
		t.Fatalf("expected error for nonexistent_type, got nil")
	}
	if !errors.Is(err, ErrNoMatchingStatus) {
		t.Errorf("expected ErrNoMatchingStatus, got: %v", err)
	}
}

func TestResolveStatus_StaleCache(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Seed stale cache entry
	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "status",
		EntityID:   "stale-status",
		ParentID:   "list-stale",
		Name:       "stale-status",
		StatusType: "closed",
		FetchedAt:  time.Now().Add(-48 * time.Hour), // 48 hours old (> 24h TTL)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-stale" {
			_, _ = w.Write([]byte(`{
				"id": "task-stale",
				"name": "Stale Task",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "list-stale", "name": "Stale List"}
			}`))
			return
		}
		if r.URL.Path == "/list/list-stale" {
			_, _ = w.Write([]byte(`{
				"id": "list-stale",
				"name": "Stale List",
				"statuses": [
					{"status": "refreshed-closed", "type": "closed", "orderindex": 0}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)

	statusName, err := ResolveStatus(ctx, st, client, "task-stale", "closed")
	if err != nil {
		t.Fatalf("ResolveStatus stale failed: %v", err)
	}
	if statusName != "refreshed-closed" {
		t.Errorf("expected refreshed status 'refreshed-closed', got %q", statusName)
	}
}

func TestResolveStatus_StaleTaskCache(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Seed stale task cache entry
	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "task",
		EntityID:   "task-stale-task",
		ParentID:   "old-list",
		Name:       "Old Task Name",
		FetchedAt:  time.Now().Add(-48 * time.Hour), // 48 hours old (> 24h TTL)
	})

	var getTaskCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-stale-task" {
			getTaskCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id": "task-stale-task",
				"name": "Refreshed Task",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "new-list", "name": "New List"}
			}`))
			return
		}
		if r.URL.Path == "/list/new-list" {
			_, _ = w.Write([]byte(`{
				"id": "new-list",
				"name": "New List",
				"statuses": [
					{"status": "Completed", "type": "closed", "orderindex": 0}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)

	statusName, err := ResolveStatus(ctx, st, client, "task-stale-task", "closed")
	if err != nil {
		t.Fatalf("ResolveStatus failed: %v", err)
	}
	if statusName != "Completed" {
		t.Errorf("expected status 'Completed', got %q", statusName)
	}
	if getTaskCalls.Load() != 1 {
		t.Errorf("expected task to be refetched on stale cache, calls: %d", getTaskCalls.Load())
	}
}

// TestResolveStatus_SharedStatusNamesAcrossLists guards against status cache
// rows for one list being overwritten by another list that shares status names.
func TestResolveStatus_SharedStatusNamesAcrossLists(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/task/task-a":
			_, _ = w.Write([]byte(`{"id": "task-a", "list": {"id": "list-a"}}`))
		case "/task/task-b":
			_, _ = w.Write([]byte(`{"id": "task-b", "list": {"id": "list-b"}}`))
		case "/list/list-a":
			_, _ = w.Write([]byte(`{"id": "list-a", "statuses": [
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "complete", "type": "done", "orderindex": 1},
				{"status": "cancelled", "type": "closed", "orderindex": 2}
			]}`))
		case "/list/list-b":
			_, _ = w.Write([]byte(`{"id": "list-b", "statuses": [
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "complete", "type": "done", "orderindex": 1}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)

	for _, taskID := range []string{"task-a", "task-b"} {
		if _, err := ResolveStatus(ctx, st, client, taskID, "done"); err != nil {
			t.Fatalf("ResolveStatus(%s) failed: %v", taskID, err)
		}
	}

	// list-a's cached statuses must survive list-b's refresh.
	statusName, err := ResolveStatus(ctx, st, client, "task-a", "done")
	if err != nil {
		t.Fatalf("ResolveStatus after other list refresh failed: %v", err)
	}
	if statusName != "complete" {
		t.Errorf("expected 'complete' for list-a, got %q", statusName)
	}

	cached, err := st.GetListStatuses(ctx, "list-a")
	if err != nil {
		t.Fatalf("GetListStatuses failed: %v", err)
	}
	if len(cached) != 3 {
		t.Errorf("expected 3 cached statuses for list-a, got %d", len(cached))
	}
}
