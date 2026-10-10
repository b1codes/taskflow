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

// openTaskStatus is a task's current status before any session starts work.
const openTaskStatus = `{"status": "to do", "type": "open"}`

// newStatusServer serves a single task, currently in status "to do", on a
// single list with the given statuses JSON, counting task and list fetches.
func newStatusServer(t *testing.T, taskID, listID, statusesJSON string, getTaskCalls, getListCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return newTaskStatusServer(t, taskID, listID, openTaskStatus, statusesJSON, getTaskCalls, getListCalls)
}

// newTaskStatusServer is newStatusServer with the task's current status given
// as JSON (e.g. {"status": "complete", "type": "done"}).
func newTaskStatusServer(t *testing.T, taskID, listID, taskStatusJSON, statusesJSON string, getTaskCalls, getListCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/task/" + taskID:
			if getTaskCalls != nil {
				getTaskCalls.Add(1)
			}
			_, _ = w.Write([]byte(`{"id": "` + taskID + `", "name": "Test Task", "status": ` + taskStatusJSON + `, "list": {"id": "` + listID + `"}}`))
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
		{"start picks the in-progress custom status", developmentTaskListStatuses, RoleStart, "in progress", nil},
		{"complete picks done, not closed", developmentTaskListStatuses, RoleComplete, "complete", nil},
		{"archive picks closed", developmentTaskListStatuses, RoleArchive, "cancelled", nil},
		{
			name: "start prefers a working status over an earlier review status",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "in review", "type": "custom", "orderindex": 1},
				{"status": "doing", "type": "custom", "orderindex": 2},
				{"status": "complete", "type": "done", "orderindex": 3}
			]`,
			role: RoleStart,
			want: "doing",
		},
		{
			name: "start skips review, blocked and QA stages",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "in review", "type": "custom", "orderindex": 1},
				{"status": "blocked", "type": "custom", "orderindex": 2},
				{"status": "building", "type": "custom", "orderindex": 3}
			]`,
			role: RoleStart,
			want: "building",
		},
		{
			name:     "start skips when every custom status is a later stage",
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "in review", "type": "custom", "orderindex": 1}, {"status": "testing/qa", "type": "custom", "orderindex": 2}]`,
			role:     RoleStart,
			wantErr:  ErrSkipStatusChange,
		},
		{
			name:     "start skips without custom statuses",
			statuses: `[{"status": "Open", "type": "open", "orderindex": 0}, {"status": "Closed", "type": "closed", "orderindex": 1}]`,
			role:     RoleStart,
			wantErr:  ErrSkipStatusChange,
		},
		{
			name:     "complete falls back to closed without done",
			statuses: `[{"status": "Open", "type": "open", "orderindex": 0}, {"status": "Closed", "type": "closed", "orderindex": 1}]`,
			role:     RoleComplete,
			want:     "Closed",
		},
		{
			name:     "archive without closed status leaves the task alone",
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "complete", "type": "done", "orderindex": 1}]`,
			role:     RoleArchive,
			wantErr:  ErrSkipStatusChange,
		},
		{
			name:     "complete without done or closed status has no match",
			statuses: `[{"status": "to do", "type": "open", "orderindex": 0}, {"status": "doing", "type": "custom", "orderindex": 1}]`,
			role:     RoleComplete,
			wantErr:  ErrNoMatchingStatus,
		},
		{
			name:     "start respects orderindex, not response order",
			statuses: `[{"status": "shipping", "type": "custom", "orderindex": 2}, {"status": "building", "type": "custom", "orderindex": 1}]`,
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

// TestResolveStatusForRole_RespectsCurrentStatus guards against taskflow
// moving a task back from a status someone else chose: reopening a finished
// task on start, cancelling a completed task on archive, or pulling a task out
// of review.
func TestResolveStatusForRole_RespectsCurrentStatus(t *testing.T) {
	t.Parallel()

	const (
		inProgress = `{"status": "in progress", "type": "custom"}`
		inReview   = `{"status": "in review", "type": "custom"}`
		complete   = `{"status": "complete", "type": "done"}`
		cancelled  = `{"status": "cancelled", "type": "closed"}`
	)

	tests := []struct {
		name    string
		current string
		role    StatusRole
		want    string // "" means the status change is skipped
	}{
		{"start leaves a task in review", inReview, RoleStart, ""},
		{"start leaves a task already in progress", inProgress, RoleStart, ""},
		{"start doesn't reopen a completed task", complete, RoleStart, ""},
		{"start doesn't reopen a cancelled task", cancelled, RoleStart, ""},
		{"archive doesn't cancel a completed task", complete, RoleArchive, ""},
		{"archive leaves a cancelled task", cancelled, RoleArchive, ""},
		{"complete leaves a completed task", complete, RoleComplete, ""},
		{"complete moves a task out of review", inReview, RoleComplete, "complete"},
		{"archive cancels a task in progress", inProgress, RoleArchive, "cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var getListCalls atomic.Int32
			server := newTaskStatusServer(t, "task-1", "list-1", tt.current, developmentTaskListStatuses, nil, &getListCalls)
			client := New("api-key").WithBaseURL(server.URL)

			got, err := ResolveStatusForRole(context.Background(), newMemStore(t), client, "task-1", tt.role, nil)
			if tt.want == "" {
				if !errors.Is(err, ErrSkipStatusChange) {
					t.Fatalf("expected ErrSkipStatusChange, got %q, %v", got, err)
				}
				if getListCalls.Load() != 0 {
					t.Errorf("expected no list fetch for a skipped role, got %d", getListCalls.Load())
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

	got, err := ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, map[StatusRole]string{
		RoleStart: "Testing/QA", // case-insensitive match, returns list's casing
	})
	if err != nil {
		t.Fatalf("ResolveStatusForRole(start) failed: %v", err)
	}
	if got != "testing/qa" {
		t.Errorf("expected override 'testing/qa', got %q", got)
	}

	// Override missing from the list falls back to the default.
	got, err = ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, map[StatusRole]string{
		RoleStart: "awaiting sign-off",
	})
	if err != nil {
		t.Fatalf("ResolveStatusForRole(start) with missing override failed: %v", err)
	}
	if got != "in progress" {
		t.Errorf("expected default 'in progress' after missing override, got %q", got)
	}
}

func TestResolveStatusForRole_UnknownRole(t *testing.T) {
	t.Parallel()

	server := newStatusServer(t, "task-1", "list-1", developmentTaskListStatuses, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)

	_, err := ResolveStatusForRole(context.Background(), nil, client, "task-1", StatusRole("review"), nil)
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
	// 2. Statuses come from the cache; the task is always fetched live for
	// its current status.
	if got, err := ResolveStatusForRole(ctx, st, client, "task-123", RoleStart, nil); err != nil || got != "in progress" {
		t.Fatalf("expected 'in progress', got %q, %v", got, err)
	}
	if getTaskCalls.Load() != 2 || getListCalls.Load() != 1 {
		t.Errorf("expected 2 task and 1 list fetch, got %d and %d", getTaskCalls.Load(), getListCalls.Load())
	}
}

func TestResolveStatusForRole_StaleStatusCache(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()

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
	if cached, _ := st.GetListStatuses(ctx, "list-stale"); len(cached) != 1 || cached[0].Name != "refreshed-complete" {
		t.Errorf("expected the stale status to be replaced in the cache, got %v", cached)
	}
}

func TestResolveStatusForRole_TaskMovedToAnotherList(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()

	// Fresh statuses cached for the list the task used to be on.
	_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
		EntityType: "status",
		EntityID:   statusCacheID("old-list", "done-old"),
		ParentID:   "old-list",
		Name:       "done-old",
		StatusType: "done",
		FetchedAt:  time.Now(),
	})

	server := newStatusServer(t, "task-moved", "new-list",
		`[{"status": "Completed", "type": "done", "orderindex": 0}]`, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(ctx, st, client, "task-moved", RoleComplete, nil)
	if err != nil {
		t.Fatalf("ResolveStatusForRole failed: %v", err)
	}
	if got != "Completed" {
		t.Errorf("expected 'Completed' from the task's new list, got %q", got)
	}
}

// failingDeleteStore fails DeleteCacheEntries, as a SQLITE_BUSY would when
// serve and `taskflow sync` share the database.
type failingDeleteStore struct {
	store.Store
	err error
}

func (s failingDeleteStore) DeleteCacheEntries(context.Context, string, string) error {
	return s.err
}

func TestResolveStatusForRole_CacheClearFailureIsReported(t *testing.T) {
	t.Parallel()

	errBusy := errors.New("database is locked")
	st := failingDeleteStore{Store: newMemStore(t), err: errBusy}
	server := newStatusServer(t, "task-1", "list-1", developmentTaskListStatuses, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)

	_, err := ResolveStatusForRole(context.Background(), st, client, "task-1", RoleStart, nil)
	if !errors.Is(err, errBusy) {
		t.Fatalf("expected the cache clear error, got %v", err)
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
			_, _ = w.Write([]byte(`{"id": "task-a", "status": ` + openTaskStatus + `, "list": {"id": "list-a"}}`))
		case "/task/task-b":
			_, _ = w.Write([]byte(`{"id": "task-b", "status": ` + openTaskStatus + `, "list": {"id": "list-b"}}`))
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
