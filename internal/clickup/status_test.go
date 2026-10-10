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

// developmentTaskListStatuses mirrors a real ClickUp list (checked against
// the API on 2026-10-10). ClickUp status types are only open/custom/done/closed.
const developmentTaskListStatuses = `[
	{"status": "to do", "type": "open", "orderindex": 0},
	{"status": "in progress", "type": "custom", "orderindex": 1},
	{"status": "in review", "type": "custom", "orderindex": 2},
	{"status": "testing/qa", "type": "custom", "orderindex": 3},
	{"status": "complete", "type": "done", "orderindex": 4},
	{"status": "cancelled", "type": "closed", "orderindex": 5}
]`

// newStatusServer serves a single task on a single list with the given
// statuses JSON, counting task and list fetches.
func newStatusServer(t *testing.T, taskID, listID, statusesJSON string, getTaskCalls, getListCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/task/" + taskID:
			if getTaskCalls != nil {
				getTaskCalls.Add(1)
			}
			_, _ = w.Write([]byte(`{"id": "` + taskID + `", "name": "Test Task", "list": {"id": "` + listID + `"}}`))
		case "/list/" + listID:
			if getListCalls != nil {
				getListCalls.Add(1)
			}
			_, _ = w.Write([]byte(`{"id": "` + listID + `", "statuses": ` + statusesJSON + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newMemStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestResolveStatusForRole_Defaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		statuses string
		role     StatusRole
		want     string
		wantErr  error
	}{
		{"start picks first custom", developmentTaskListStatuses, RoleStart, "in progress", nil},
		{"review picks custom named review", developmentTaskListStatuses, RoleReview, "in review", nil},
		{"complete picks done, not closed", developmentTaskListStatuses, RoleComplete, "complete", nil},
		{"archive picks closed", developmentTaskListStatuses, RoleArchive, "cancelled", nil},
		{
			name:     "start falls back to open without custom statuses",
			statuses: `[{"status": "Open", "type": "open", "orderindex": 0}, {"status": "Closed", "type": "closed", "orderindex": 1}]`,
			role:     RoleStart,
			want:     "Open",
		},
		{
			name:     "complete falls back to closed without done",
			statuses: `[{"status": "Open", "type": "open", "orderindex": 0}, {"status": "Closed", "type": "closed", "orderindex": 1}]`,
			role:     RoleComplete,
			want:     "Closed",
		},
		{
			name:     "review skips when no status mentions review",
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "doing", "type": "custom", "orderindex": 1}]`,
			role:     RoleReview,
			wantErr:  ErrSkipStatusChange,
		},
		{
			name:     "archive without closed status has no match",
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "complete", "type": "done", "orderindex": 1}]`,
			role:     RoleArchive,
			wantErr:  ErrNoMatchingStatus,
		},
		{
			name:     "start respects orderindex, not response order",
			statuses: `[{"status": "qa", "type": "custom", "orderindex": 2}, {"status": "building", "type": "custom", "orderindex": 1}]`,
			role:     RoleStart,
			want:     "building",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newStatusServer(t, "task-1", "list-1", tt.statuses, nil, nil)
			client := New("api-key").WithBaseURL(server.URL)

			got, err := ResolveStatusForRole(context.Background(), newMemStore(t), client, "task-1", tt.role, nil)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %q, %v", tt.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveStatusForRole failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestResolveStatusForRole_Overrides(t *testing.T) {
	t.Parallel()

	server := newStatusServer(t, "task-1", "list-1", developmentTaskListStatuses, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)
	st := newMemStore(t)
	ctx := context.Background()

	overrides := map[StatusRole]string{
		RoleStart:  "Testing/QA", // case-insensitive match, returns list's casing
		RoleReview: "awaiting sign-off",
	}

	got, err := ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, overrides)
	if err != nil {
		t.Fatalf("ResolveStatusForRole(start) failed: %v", err)
	}
	if got != "testing/qa" {
		t.Errorf("expected override 'testing/qa', got %q", got)
	}

	// Override missing from the list falls back to the default.
	got, err = ResolveStatusForRole(ctx, st, client, "task-1", RoleReview, overrides)
	if err != nil {
		t.Fatalf("ResolveStatusForRole(review) failed: %v", err)
	}
	if got != "in review" {
		t.Errorf("expected default 'in review' after missing override, got %q", got)
	}
}

func TestResolveStatusForRole_UnknownRole(t *testing.T) {
	t.Parallel()

	_, err := ResolveStatusForRole(context.Background(), nil, New("api-key"), "task-1", StatusRole("active"), nil)
	if !errors.Is(err, ErrUnknownStatusRole) {
		t.Fatalf("expected ErrUnknownStatusRole, got %v", err)
	}
}

func TestLegacyStatusRole(t *testing.T) {
	t.Parallel()

	for statusType, want := range map[string]StatusRole{"active": RoleStart, "closed": RoleComplete} {
		got, err := LegacyStatusRole(statusType)
		if err != nil || got != want {
			t.Errorf("LegacyStatusRole(%q) = %q, %v; want %q", statusType, got, err, want)
		}
	}
	if _, err := LegacyStatusRole("open"); !errors.Is(err, ErrUnknownStatusRole) {
		t.Errorf("expected ErrUnknownStatusRole for unmapped legacy type, got %v", err)
	}
}

func TestResolveStatusForRole_UsesCache(t *testing.T) {
	t.Parallel()

	var getTaskCalls, getListCalls atomic.Int32
	server := newStatusServer(t, "task-123", "list-abc", developmentTaskListStatuses, &getTaskCalls, &getListCalls)
	client := New("api-key").WithBaseURL(server.URL)
	st := newMemStore(t)
	ctx := context.Background()

	// 1. Empty cache: fetches task and list.
	if got, err := ResolveStatusForRole(ctx, st, client, "task-123", RoleComplete, nil); err != nil || got != "complete" {
		t.Fatalf("expected 'complete', got %q, %v", got, err)
	}
	// 2. Cache hit: no further API calls.
	if got, err := ResolveStatusForRole(ctx, st, client, "task-123", RoleStart, nil); err != nil || got != "in progress" {
		t.Fatalf("expected 'in progress', got %q, %v", got, err)
	}
	if getTaskCalls.Load() != 1 || getListCalls.Load() != 1 {
		t.Errorf("expected 1 task and 1 list fetch, got %d and %d", getTaskCalls.Load(), getListCalls.Load())
	}
}

func TestResolveStatusForRole_StaleStatusCache(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()

	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "task",
		EntityID:   "task-stale",
		ParentID:   "list-stale",
		FetchedAt:  time.Now(),
	})
	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "status",
		EntityID:   statusCacheID("list-stale", "stale-status"),
		ParentID:   "list-stale",
		Name:       "stale-status",
		StatusType: "done",
		FetchedAt:  time.Now().Add(-48 * time.Hour), // older than the 24h TTL
	})

	server := newStatusServer(t, "task-stale", "list-stale",
		`[{"status": "refreshed-complete", "type": "done", "orderindex": 0}]`, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(ctx, st, client, "task-stale", RoleComplete, nil)
	if err != nil {
		t.Fatalf("ResolveStatusForRole failed: %v", err)
	}
	if got != "refreshed-complete" {
		t.Errorf("expected refreshed status 'refreshed-complete', got %q", got)
	}
}

func TestResolveStatusForRole_StaleTaskCache(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()

	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "task",
		EntityID:   "task-moved",
		ParentID:   "old-list",
		FetchedAt:  time.Now().Add(-48 * time.Hour),
	})

	var getTaskCalls atomic.Int32
	server := newStatusServer(t, "task-moved", "new-list",
		`[{"status": "Completed", "type": "done", "orderindex": 0}]`, &getTaskCalls, nil)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(ctx, st, client, "task-moved", RoleComplete, nil)
	if err != nil {
		t.Fatalf("ResolveStatusForRole failed: %v", err)
	}
	if got != "Completed" {
		t.Errorf("expected 'Completed' from the task's new list, got %q", got)
	}
	if getTaskCalls.Load() != 1 {
		t.Errorf("expected task to be refetched on stale cache, calls: %d", getTaskCalls.Load())
	}
}

// TestResolveStatusForRole_SharedStatusNamesAcrossLists guards against status
// cache rows for one list being overwritten by another list that shares
// status names, which previously left a list resolving "complete" to its
// closed ("cancelled") status.
func TestResolveStatusForRole_SharedStatusNamesAcrossLists(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
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
		if _, err := ResolveStatusForRole(ctx, st, client, taskID, RoleComplete, nil); err != nil {
			t.Fatalf("ResolveStatusForRole(%s) failed: %v", taskID, err)
		}
	}

	// list-a's cached statuses must survive list-b's refresh.
	got, err := ResolveStatusForRole(ctx, st, client, "task-a", RoleComplete, nil)
	if err != nil {
		t.Fatalf("ResolveStatusForRole after other list refresh failed: %v", err)
	}
	if got != "complete" {
		t.Errorf("expected 'complete' for list-a, got %q", got)
	}

	cached, err := st.GetListStatuses(ctx, "list-a")
	if err != nil {
		t.Fatalf("GetListStatuses failed: %v", err)
	}
	if len(cached) != 3 {
		t.Errorf("expected 3 cached statuses for list-a, got %d", len(cached))
	}
}
