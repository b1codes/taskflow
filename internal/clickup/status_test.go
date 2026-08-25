package clickup

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-123" {
			_, _ = w.Write([]byte(`{
				"id": "task-123",
				"name": "Test Task",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "list-abc", "name": "Sprint 1"}
			}`))
			return
		}
		if r.URL.Path == "/list/list-abc" {
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

	// 1. Initial resolution with empty cache (triggers refresh from API)
	statusName, err := ResolveStatus(ctx, st, client, "task-123", "closed")
	if err != nil {
		t.Fatalf("ResolveStatus failed: %v", err)
	}
	if statusName != "done" {
		t.Errorf("expected status 'done' for type 'closed', got %q", statusName)
	}

	// 2. Subsequent resolution (cache hit)
	statusActive, err := ResolveStatus(ctx, st, client, "task-123", "active")
	if err != nil {
		t.Fatalf("ResolveStatus cache hit failed: %v", err)
	}
	if statusActive != "in progress" {
		t.Errorf("expected status 'in progress' for type 'active', got %q", statusActive)
	}

	// 3. Unknown target type
	unknownStatus, err := ResolveStatus(ctx, st, client, "task-123", "nonexistent_type")
	if err != nil {
		t.Fatalf("ResolveStatus unknown type failed: %v", err)
	}
	if unknownStatus != "" {
		t.Errorf("expected empty status for unknown type, got %q", unknownStatus)
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
