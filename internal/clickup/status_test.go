package clickup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// clickUpDefaultListStatuses mirrors the statuses ClickUp gives a new list,
// where finished tasks go to the closed status "complete".
const clickUpDefaultListStatuses = `[
	{"status": "to do", "type": "open", "orderindex": 0},
	{"status": "in progress", "type": "custom", "orderindex": 1},
	{"status": "complete", "type": "closed", "orderindex": 2}
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
		{"reopen picks the in-progress custom status", developmentTaskListStatuses, RoleReopen, "in progress", nil},
		{"archive skips ClickUp's default closed status", clickUpDefaultListStatuses, RoleArchive, "", ErrSkipStatusChange},
		{"complete falls back to ClickUp's default closed status", clickUpDefaultListStatuses, RoleComplete, "complete", nil},
		{
			name: "archive picks a closed status named like a cancellation",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "complete", "type": "closed", "orderindex": 1},
				{"status": "Won't Do", "type": "closed", "orderindex": 2}
			]`,
			role: RoleArchive,
			want: "Won't Do",
		},
		{
			name:     "archive matches word prefixes",
			statuses: `[{"status": "Closed", "type": "closed", "orderindex": 0}, {"status": "Archived", "type": "closed", "orderindex": 1}]`,
			role:     RoleArchive,
			want:     "Archived",
		},
		{
			name:     "archive ignores done statuses named like a cancellation",
			statuses: `[{"status": "dropped", "type": "done", "orderindex": 0}, {"status": "closed", "type": "closed", "orderindex": 1}]`,
			role:     RoleArchive,
			wantErr:  ErrSkipStatusChange,
		},
		{
			name: "start skips a pre-work status that names a working hint",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "ready for development", "type": "custom", "orderindex": 1},
				{"status": "in progress", "type": "custom", "orderindex": 2}
			]`,
			role: RoleStart,
			want: "in progress",
		},
		{
			name: "start matches whole words, not substrings",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "inactive", "type": "custom", "orderindex": 1},
				{"status": "doing", "type": "custom", "orderindex": 2}
			]`,
			role: RoleStart,
			want: "doing",
		},
		{
			name: "start tries hints in priority order across the list",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "doing", "type": "custom", "orderindex": 1},
				{"status": "in progress", "type": "custom", "orderindex": 2}
			]`,
			role: RoleStart,
			want: "in progress",
		},
		{
			name: "start fallback skips pre-work custom statuses",
			statuses: `[
				{"status": "to do", "type": "open", "orderindex": 0},
				{"status": "backlog", "type": "custom", "orderindex": 1},
				{"status": "Up Next", "type": "custom", "orderindex": 2},
				{"status": "To-Do", "type": "custom", "orderindex": 3},
				{"status": "building", "type": "custom", "orderindex": 4}
			]`,
			role: RoleStart,
			want: "building",
		},
		{
			name:     "start skips when only pre-work and later stages are custom",
			statuses: `[{"status": "backlog", "type": "custom", "orderindex": 0}, {"status": "in review", "type": "custom", "orderindex": 1}]`,
			role:     RoleStart,
			wantErr:  ErrSkipStatusChange,
		},
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
		{"reopen moves a completed task back to work", complete, RoleReopen, "in progress"},
		{"reopen moves a cancelled task back to work", cancelled, RoleReopen, "in progress"},
		{"reopen starts an open task", openTaskStatus, RoleReopen, "in progress"},
		{"reopen leaves a task in review", inReview, RoleReopen, ""},
		{"reopen leaves a task already in progress", inProgress, RoleReopen, ""},
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

// failingDeleteStore fails ReplaceListStatuses, as a SQLITE_BUSY would when
// serve and `taskflow sync` share the database.
type failingDeleteStore struct {
	store.Store
	err error
}

func (s failingDeleteStore) ReplaceListStatuses(context.Context, string, []*store.CacheEntry) error {
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

func TestResolveStatusForRole_ArchiveOverrideWins(t *testing.T) {
	t.Parallel()

	server := newStatusServer(t, "task-1", "list-1", clickUpDefaultListStatuses, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(context.Background(), newMemStore(t), client, "task-1", RoleArchive, map[StatusRole]string{
		RoleArchive: "Complete",
	})
	if err != nil {
		t.Fatalf("ResolveStatusForRole(archive) failed: %v", err)
	}
	if got != "complete" {
		t.Errorf("expected override 'complete', got %q", got)
	}
}

func TestResolveStatusForRole_ReopenOverrides(t *testing.T) {
	t.Parallel()

	server := newTaskStatusServer(t, "task-1", "list-1", `{"status": "complete", "type": "done"}`, developmentTaskListStatuses, nil, nil)
	client := New("api-key").WithBaseURL(server.URL)
	st := newMemStore(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		overrides map[StatusRole]string
		want      string
	}{
		{"falls back to the start override", map[StatusRole]string{RoleStart: "testing/qa"}, "testing/qa"},
		{"prefers its own override", map[StatusRole]string{RoleStart: "testing/qa", RoleReopen: "In Review"}, "in review"},
	}
	for _, tt := range tests {
		got, err := ResolveStatusForRole(ctx, st, client, "task-1", RoleReopen, tt.overrides)
		if err != nil {
			t.Fatalf("%s: ResolveStatusForRole(reopen) failed: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: expected %q, got %q", tt.name, tt.want, got)
		}
	}
}

// seedStatusCache caches statuses for listID as freshly fetched.
func seedStatusCache(t *testing.T, st store.Store, listID, statusesJSON string) {
	t.Helper()
	var statuses []ListStatus
	if err := json.Unmarshal([]byte(statusesJSON), &statuses); err != nil {
		t.Fatalf("bad statuses JSON: %v", err)
	}
	if err := cacheListStatuses(context.Background(), st, listID, statusEntries(listID, statuses)); err != nil {
		t.Fatalf("failed to seed status cache: %v", err)
	}
}

// TestResolveStatusForRole_RefreshesCacheMissingCurrentStatus guards against
// resolving against a young cache that provably predates a status change in
// ClickUp: the task sits in a status the cached list doesn't have.
func TestResolveStatusForRole_RefreshesCacheMissingCurrentStatus(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()
	seedStatusCache(t, st, "list-1", `[
		{"status": "to do", "type": "open", "orderindex": 0},
		{"status": "doing", "type": "custom", "orderindex": 1}
	]`)

	var getListCalls atomic.Int32
	server := newTaskStatusServer(t, "task-1", "list-1", `{"status": "triage", "type": "open"}`, `[
		{"status": "triage", "type": "open", "orderindex": 0},
		{"status": "in progress", "type": "custom", "orderindex": 1}
	]`, nil, &getListCalls)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, nil)
	if err != nil {
		t.Fatalf("ResolveStatusForRole failed: %v", err)
	}
	if got != "in progress" {
		t.Errorf("expected 'in progress' from the refreshed list, got %q", got)
	}
	if getListCalls.Load() != 1 {
		t.Errorf("expected 1 list fetch, got %d", getListCalls.Load())
	}
	cached, _ := st.GetListStatuses(ctx, "list-1")
	if len(cached) != 2 || cached[0].Name != "triage" {
		t.Errorf("expected the cache to be replaced with the refreshed statuses, got %v", cached)
	}
}

// TestResolveStatusForRole_OverrideRefreshesCache checks that an override
// missing from a cached list is looked up once more in a fresh copy before
// falling back to the default.
func TestResolveStatusForRole_OverrideRefreshesCache(t *testing.T) {
	t.Parallel()

	st := newMemStore(t)
	ctx := context.Background()
	seedStatusCache(t, st, "list-1", `[
		{"status": "to do", "type": "open", "orderindex": 0},
		{"status": "in progress", "type": "custom", "orderindex": 1}
	]`)

	var getListCalls atomic.Int32
	server := newStatusServer(t, "task-1", "list-1", `[
		{"status": "to do", "type": "open", "orderindex": 0},
		{"status": "in progress", "type": "custom", "orderindex": 1},
		{"status": "building", "type": "custom", "orderindex": 2}
	]`, nil, &getListCalls)
	client := New("api-key").WithBaseURL(server.URL)

	got, err := ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, map[StatusRole]string{RoleStart: "Building"})
	if err != nil {
		t.Fatalf("ResolveStatusForRole failed: %v", err)
	}
	if got != "building" {
		t.Errorf("expected override 'building' after refresh, got %q", got)
	}
	if getListCalls.Load() != 1 {
		t.Errorf("expected 1 list fetch, got %d", getListCalls.Load())
	}

	// A missing override refreshes once, then falls back to the default.
	// The name is unique to this test: the known-missing set is process-wide.
	missing := map[StatusRole]string{RoleStart: "missing " + t.Name()}
	forgetOverrideMissing(t, "list-1", RoleStart, missing[RoleStart])
	getListCalls.Store(0)
	got, err = ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, missing)
	if err != nil {
		t.Fatalf("ResolveStatusForRole with missing override failed: %v", err)
	}
	if got != "in progress" {
		t.Errorf("expected default 'in progress', got %q", got)
	}
	if getListCalls.Load() != 1 {
		t.Errorf("expected 1 list fetch for a missing override, got %d", getListCalls.Load())
	}

	// Once confirmed missing, later ops use the cache without refetching.
	getListCalls.Store(0)
	if got, err = ResolveStatusForRole(ctx, st, client, "task-1", RoleStart, missing); err != nil || got != "in progress" {
		t.Fatalf("expected default 'in progress', got %q, %v", got, err)
	}
	if getListCalls.Load() != 0 {
		t.Errorf("expected no list fetch for a known-missing override, got %d", getListCalls.Load())
	}
}

// forgetOverrideMissing removes the override from the process-wide
// known-missing set when the test ends, so `go test -count=N` reruns start
// clean.
func forgetOverrideMissing(t *testing.T, listID string, key StatusRole, want string) {
	t.Cleanup(func() { warnedOverrides.Delete(overrideWarnKey(listID, key, want)) })
}

func TestWarnOverrideMissing_OncePerListRoleAndName(t *testing.T) {
	t.Parallel()

	list := t.Name() // unique to this test; the dedupe set is process-wide
	steps := []struct {
		listID string
		role   StatusRole
		name   string
		want   bool
	}{
		{list, RoleStart, "doing", true},
		{list, RoleStart, "doing", false},
		{list, RoleStart, "building", true},
		{list, RoleReopen, "doing", true},
		{list + "-other", RoleStart, "doing", true},
	}
	for i, s := range steps {
		forgetOverrideMissing(t, s.listID, s.role, s.name)
		if got := warnOverrideMissing(s.listID, s.role, s.name); got != s.want {
			t.Errorf("step %d: warnOverrideMissing(%q, %q, %q) = %v, want %v", i, s.listID, s.role, s.name, got, s.want)
		}
	}
}

// fakeClickUp serves one task on one list. Like ClickUp, it accepts a status
// update only to a status the list has and answers 400 otherwise (or always,
// with rejectUpdates). It counts fetches and records every update.
type fakeClickUp struct {
	taskStatusJSON string
	statusesJSON   string
	rejectUpdates  bool

	getTaskCalls, getListCalls atomic.Int32

	mu      sync.Mutex
	updates []string
}

func (f *fakeClickUp) start(t *testing.T) *Client {
	t.Helper()
	var statuses []ListStatus
	if err := json.Unmarshal([]byte(f.statusesJSON), &statuses); err != nil {
		t.Fatalf("bad statuses JSON: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/task/task-1" && r.Method == http.MethodPut:
			var req updateStatusRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.updates = append(f.updates, req.Status)
			f.mu.Unlock()
			known := slices.ContainsFunc(statuses, func(s ListStatus) bool { return strings.EqualFold(s.Status, req.Status) })
			if f.rejectUpdates || !known {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"err": "Status not found", "ECODE": "CRTSK_001"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/task/task-1":
			f.getTaskCalls.Add(1)
			_, _ = w.Write([]byte(`{"id": "task-1", "status": ` + f.taskStatusJSON + `, "list": {"id": "list-1"}}`))
		case r.URL.Path == "/list/list-1":
			f.getListCalls.Add(1)
			_, _ = w.Write([]byte(`{"id": "list-1", "statuses": ` + f.statusesJSON + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return New("api-key").WithBaseURL(server.URL)
}

func (f *fakeClickUp) updateNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.updates)
}

// renamedStatusCache is list-1 as cached before "doing" was renamed to "in
// progress" in ClickUp (developmentTaskListStatuses).
const renamedStatusCache = `[
	{"status": "to do", "type": "open", "orderindex": 0},
	{"status": "doing", "type": "custom", "orderindex": 1}
]`

func TestApplyStatusRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		cached        string // seeded as fresh; "" leaves the cache empty
		current       string
		role          StatusRole
		rejectUpdates bool
		wantUpdates   []string
		wantErr       error
		wantListCalls int32
	}{
		{
			name:          "moves the task",
			current:       openTaskStatus,
			role:          RoleStart,
			wantUpdates:   []string{"in progress"},
			wantListCalls: 1,
		},
		{
			name:          "refreshes a stale cache and retries once after a 400",
			cached:        renamedStatusCache,
			current:       openTaskStatus,
			role:          RoleStart,
			wantUpdates:   []string{"doing", "in progress"},
			wantListCalls: 1,
		},
		{
			name:          "returns the retry's error when it is rejected too",
			cached:        renamedStatusCache,
			current:       openTaskStatus,
			role:          RoleStart,
			rejectUpdates: true,
			wantUpdates:   []string{"doing", "in progress"},
			wantErr:       ErrBadRequest,
			wantListCalls: 1,
		},
		{
			name:          "doesn't retry a 400 against freshly fetched statuses",
			current:       openTaskStatus,
			role:          RoleStart,
			rejectUpdates: true,
			wantUpdates:   []string{"in progress"},
			wantErr:       ErrBadRequest,
			wantListCalls: 1,
		},
		{
			name:        "skips without updating",
			current:     `{"status": "complete", "type": "done"}`,
			role:        RoleStart,
			wantUpdates: nil,
			wantErr:     ErrSkipStatusChange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := newMemStore(t)
			if tt.cached != "" {
				seedStatusCache(t, st, "list-1", tt.cached)
			}
			fake := &fakeClickUp{taskStatusJSON: tt.current, statusesJSON: developmentTaskListStatuses, rejectUpdates: tt.rejectUpdates}
			client := fake.start(t)

			err := ApplyStatusRole(context.Background(), st, client, "task-1", tt.role, nil)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("ApplyStatusRole failed: %v", err)
			}
			if got := fake.updateNames(); !slices.Equal(got, tt.wantUpdates) {
				t.Errorf("expected updates %q, got %q", tt.wantUpdates, got)
			}
			if got := fake.getTaskCalls.Load(); got != 1 {
				t.Errorf("expected the task to be fetched once, got %d", got)
			}
			if got := fake.getListCalls.Load(); got != tt.wantListCalls {
				t.Errorf("expected %d list fetches, got %d", tt.wantListCalls, got)
			}
		})
	}
}
