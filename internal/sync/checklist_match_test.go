package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
)

func TestMatchChecklistItems(t *testing.T) {
	t.Parallel()
	ci := func(id, name string) clickup.ChecklistItem { return clickup.ChecklistItem{ID: id, Name: name} }
	tests := []struct {
		name     string
		existing []clickup.ChecklistItem
		want     []session.ChecklistItem
		history  map[string][]string // nil: earlier must not be called
		noLookup bool                // earlier is nil, as with no store
		expect   map[int]string      // want index -> existing item ID
	}{
		{
			name:   "first op ever: nothing to match, no history read",
			want:   []session.ChecklistItem{item("K", "[r] A", "", false)},
			expect: map[int]string{},
		},
		{
			name:     "exact names need no history",
			existing: []clickup.ChecklistItem{ci("a", "[r] A — unit ✓"), ci("b", "[r] B")},
			want:     []session.ChecklistItem{item("K2", "[r] B", "", false), item("K1", "[r] A", "unit ✓", true)},
			expect:   map[int]string{0: "b", 1: "a"},
		},
		{
			// A void item never created has nothing to find, so it doesn't
			// send every sync through the history read.
			name:     "a void item never created needs no history",
			existing: []clickup.ChecklistItem{ci("a", "[r] A")},
			want:     []session.ChecklistItem{item("K1", "[r] A", "", false), voidItem("K2", "[r] B")},
			expect:   map[int]string{0: "a"},
		},
		{
			name:     "a void item with an item left over is looked for",
			existing: []clickup.ChecklistItem{ci("a", "[r] A"), ci("b", "[r] B")},
			want:     []session.ChecklistItem{item("K1", "[r] A", "", false), voidItem("K2", "[r] B")},
			history:  map[string][]string{},
			expect:   map[int]string{0: "a", 1: "b"},
		},
		{
			name:     "status change without history",
			existing: []clickup.ChecklistItem{ci("a", "[r] A")},
			want:     []session.ChecklistItem{item("K", "[r] A", "unit ✓", true)},
			history:  map[string][]string{},
			expect:   map[int]string{0: "a"},
		},
		{
			name:     "status change with no store to read history from",
			existing: []clickup.ChecklistItem{ci("a", "[r] A")},
			want:     []session.ChecklistItem{item("K", "[r] A", "unit ✓", true)},
			noLookup: true,
			expect:   map[int]string{0: "a"},
		},
		{
			name:     "status change with history",
			existing: []clickup.ChecklistItem{ci("a", "[r] A")},
			want:     []session.ChecklistItem{item("K", "[r] A", "unit ✓", true)},
			history:  map[string][]string{"K": {"[r] A"}},
			expect:   map[int]string{0: "a"},
		},
		{
			// K1's describe changed A→B and a new K2 renders A: K1 keeps its
			// item, K2 gets a new one.
			name:     "an edited Key keeps its item from a new Key rendering its old text",
			existing: []clickup.ChecklistItem{ci("a", "[r] A — unit ✓")},
			want:     []session.ChecklistItem{item("K2", "[r] A", "", false), item("K1", "[r] B", "unit ✓", true)},
			history:  map[string][]string{"K1": {"[r] A"}},
			expect:   map[int]string{1: "a"},
		},
		{
			// The same, with the new Key's name exactly the old item's.
			name:     "an edited Key keeps its item even from an exact name",
			existing: []clickup.ChecklistItem{ci("a", "[r] A")},
			want:     []session.ChecklistItem{item("K2", "[r] A", "", false), item("K1", "[r] B", "", false)},
			history:  map[string][]string{"K1": {"[r] A"}},
			expect:   map[int]string{1: "a"},
		},
		{
			// K1 went X→Y in an earlier op, and K2 has rendered X since.
			name:     "a Key takes its latest item over an older one now another Key's",
			existing: []clickup.ChecklistItem{ci("x", "[r] X"), ci("y", "[r] Y")},
			want:     []session.ChecklistItem{item("K1", "[r] Y", "unit ✓", true), item("K2", "[r] X", "unit ✓", true)},
			history:  map[string][]string{"K1": {"[r] Y", "[r] X"}, "K2": {"[r] X"}},
			expect:   map[int]string{0: "y", 1: "x"},
		},
		{
			// K1's Y item is gone: its older X claim loses to K2's latest.
			name:     "a Key's latest claim beats another Key's older one",
			existing: []clickup.ChecklistItem{ci("x", "[r] X")},
			want:     []session.ChecklistItem{item("K1", "[r] Y", "unit ✓", true), item("K2", "[r] X", "unit ✓", true)},
			history:  map[string][]string{"K1": {"[r] Y", "[r] X"}, "K2": {"[r] X"}},
			expect:   map[int]string{1: "x"},
		},
		{
			name:     "within a stable part, the item of exactly the wanted name is preferred",
			existing: []clickup.ChecklistItem{ci("p", "[r] B"), ci("q", "[r] B — unit ✓")},
			want:     []session.ChecklistItem{item("K1", "[r] B", "unit ✓", true), item("K2", "[r] B", "waived", true)},
			history:  map[string][]string{"K1": {"[r] B"}, "K2": {"[r] B"}},
			expect:   map[int]string{0: "q", 1: "p"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var earlier func() map[string][]string
			if !tt.noLookup {
				earlier = func() map[string][]string {
					if tt.history == nil {
						t.Error("history read when it isn't needed")
					}
					return tt.history
				}
			}
			got := matchChecklistItems(tt.existing, tt.want, earlier)
			if len(got) != len(tt.expect) {
				t.Errorf("matched = %+v, want %v", got, tt.expect)
			}
			for i, id := range tt.expect {
				if got[i].ID != id {
					t.Errorf("want[%d] matched %+v, want item %s", i, got[i], id)
				}
			}
		})
	}
}

// checklistItems returns the task's only checklist's items, failing otherwise.
func checklistItems(t *testing.T, fake *fakeChecklists) ([]clickup.ChecklistItem, []string) {
	t.Helper()
	cls, reqs := fake.state()
	if len(cls) != 1 {
		t.Fatalf("checklists = %+v", cls)
	}
	return cls[0].Items, reqs
}

func expectItems(t *testing.T, got, want []clickup.ChecklistItem) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWorker_SyncChecklist_EditedKeyKeepsItsItem(t *testing.T) {
	t.Parallel()
	const task = "task-cl-edited-key"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("K1", "[r] A", "", false))
	drain(t, st, client)
	fake.state()

	// K1's describe changed A→B, and a new K2 renders A.
	enqueueChecklist(t, st, task,
		item("K2", "[r] A", "", false),
		item("K1", "[r] B", "", false),
	)
	drain(t, st, client)
	items, reqs := checklistItems(t, fake)
	expectItems(t, items, []clickup.ChecklistItem{
		{ID: "item-2", Name: "[r] B"},
		{ID: "item-3", Name: "[r] A"},
	})
	if countPrefix(reqs, "POST /checklist/") != 1 || countPrefix(reqs, "PUT ") != 1 {
		t.Errorf("requests = %v", reqs)
	}

	// Both resolve: each Key finds its own item, K1 by its latest Match
	// rather than the X item it had first.
	enqueueChecklist(t, st, task,
		item("K1", "[r] B", "unit ✓", true),
		item("K2", "[r] A", "unit ✓", true),
	)
	drain(t, st, client)
	items, reqs = checklistItems(t, fake)
	expectItems(t, items, []clickup.ChecklistItem{
		{ID: "item-2", Name: "[r] B — unit ✓", Resolved: true},
		{ID: "item-3", Name: "[r] A — unit ✓", Resolved: true},
	})
	if countPrefix(reqs, "POST ") != 0 || countPrefix(reqs, "PUT ") != 2 {
		t.Errorf("requests = %v", reqs)
	}
}

func TestWorker_SyncChecklist_SkippedOpIsNoHistory(t *testing.T) {
	t.Parallel()
	const task = "task-cl-skipped-history"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("K1", "[r] A", "", false))
	drain(t, st, client)
	fake.state()

	// The op where K1 went A→B and K2 appeared is skipped for a newer one,
	// so it never gave K2 the A item: K2 must not claim it as its history.
	enqueueChecklist(t, st, task, item("K2", "[r] A", "", false), item("K1", "[r] B", "", false))
	enqueueChecklist(t, st, task, item("K2", "[r] A", "", false), item("K1", "[r] B", "unit ✓", true))
	drain(t, st, client)
	items, _ := checklistItems(t, fake)
	expectItems(t, items, []clickup.ChecklistItem{
		{ID: "item-2", Name: "[r] B — unit ✓", Resolved: true},
		{ID: "item-3", Name: "[r] A"},
	})
}

// newBusyFake is newFakeChecklists where someone else adds an item named
// other just before each item taskflow creates, so the create's response
// holds two new items.
func newBusyFake(t *testing.T, taskID, other string) (*fakeChecklists, *clickup.Client) {
	t.Helper()
	f := &fakeChecklists{t: t, taskID: taskID}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if r.Method == "POST" && len(parts) == 3 && parts[0] == "checklist" && parts[2] == "checklist_item" {
			f.mu.Lock()
			cl := f.checklist(parts[1])
			cl.Items = append(cl.Items, clickup.ChecklistItem{ID: f.id("theirs"), Name: other})
			f.mu.Unlock()
		}
		f.serve(w, r)
	}))
	t.Cleanup(server.Close)
	return f, clickup.New("my-key").WithBaseURL(server.URL)
}

func TestWorker_SyncChecklist_UnidentifiableCreatedItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolved := item("K", "[r] A", "unit ✓", true)

	t.Run("is left unresolved, never created again", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-unidentifiable"
		st := newChecklistStore(t, task)
		fake, client := newBusyFake(t, task, "Someone's item")
		// Changed beyond whitespace: neither its name nor its stable part
		// tells it apart from the item someone else added.
		fake.store = strings.ToUpper

		enqueueChecklist(t, st, task, resolved)
		if n := drain(t, st, client); n != 1 {
			t.Fatalf("a create that succeeded must not fail the op, got %d processed", n)
		}
		items, reqs := checklistItems(t, fake)
		expectItems(t, items, []clickup.ChecklistItem{
			{ID: "theirs-2", Name: "Someone's item"},
			{ID: "item-3", Name: "[R] A — UNIT ✓"},
		})
		if countPrefix(reqs, "POST /checklist/") != 1 || countPrefix(reqs, "PUT ") != 0 {
			t.Errorf("requests = %v", reqs)
		}
		if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
			t.Errorf("expected no retryable ops, got %d (err: %v)", len(ops), err)
		}
		if n, _ := st.CountPendingByTaskID(ctx, task); n != 0 {
			t.Errorf("pending = %d", n)
		}
		// PARTIAL, not DONE, so the next evaluation queues the checklist
		// again rather than taking it as applied.
		if got := checklistOpStatuses(t, st, task); len(got) != 1 || got[0] != "PARTIAL" {
			t.Errorf("op statuses = %v, want PARTIAL", got)
		}
	})

	t.Run("is resolved by the next op of the same checklist", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-readback-fails"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		// The response leaves the item out, and reading it back fails.
		fake.omitCreated = true
		fake.afterCreate = func(f *fakeChecklists) { f.reject = "GET /task/" }

		enqueueChecklist(t, st, task, resolved)
		drain(t, st, client)
		if got := checklistOpStatuses(t, st, task); len(got) != 1 || got[0] != "PARTIAL" {
			t.Fatalf("op statuses = %v, want PARTIAL", got)
		}

		fake.mu.Lock()
		fake.reject, fake.afterCreate = "", nil
		fake.mu.Unlock()
		fake.state()
		enqueueChecklist(t, st, task, resolved)
		drain(t, st, client)
		items, reqs := checklistItems(t, fake)
		expectItems(t, items, []clickup.ChecklistItem{{ID: "item-2", Name: "[r] A — unit ✓", Resolved: true}})
		if countPrefix(reqs, "POST /checklist/") != 0 {
			t.Errorf("the item must not be created again: %v", reqs)
		}
	})

	t.Run("is found by its stable part", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-created-by-match"
		st := newChecklistStore(t, task)
		fake, client := newBusyFake(t, task, "Someone's item")
		// The status suffix is dropped; the stable part survives.
		fake.store = func(name string) string {
			before, _, _ := strings.Cut(name, " — ")
			return before
		}

		enqueueChecklist(t, st, task, resolved)
		if n := drain(t, st, client); n != 1 {
			t.Fatalf("expected 1 processed op, got %d", n)
		}
		items, _ := checklistItems(t, fake)
		expectItems(t, items, []clickup.ChecklistItem{
			{ID: "theirs-2", Name: "Someone's item"},
			{ID: "item-3", Name: "[r] A", Resolved: true},
		})
	})
}

// An op that never applied (here, one that failed for good) is no evidence
// of whose an item is, so its Match doesn't let its Key take another Key's
// item.
func TestWorker_SyncChecklist_FailedOpIsNoHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const task = "task-cl-failed-history"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("K2", "[r] X", "", false))
	drain(t, st, client)
	fake.state()

	// K1 once rendered X, in an op that failed before writing anything.
	enqueueChecklist(t, st, task, item("K1", "[r] X", "", false))
	failed, err := st.LatestSyncOp(ctx, task, session.OpSyncChecklist)
	if err != nil || failed == nil {
		t.Fatalf("LatestSyncOp = %+v, %v", failed, err)
	}
	if err := st.MarkFailedPermanent(ctx, failed.ID, "rejected"); err != nil {
		t.Fatalf("MarkFailedPermanent failed: %v", err)
	}

	// K1 comes first, so it would claim X first if its failed op counted.
	enqueueChecklist(t, st, task, item("K1", "[r] Y", "", false), item("K2", "[r] X", "", false))
	drain(t, st, client)
	items, _ := checklistItems(t, fake)
	expectItems(t, items, []clickup.ChecklistItem{
		{ID: "item-2", Name: "[r] X"},
		{ID: "item-3", Name: "[r] Y"},
	})
}

// The worker applies the payload of the op as it claims it, not as it was
// listed: a newer state may have been coalesced into it in between.
func TestWorker_AppliesClaimedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const task = "task-cl-claimed"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("K", "[r] A", "", false))
	listed, err := st.DequeuePending(ctx, 1)
	if err != nil || len(listed) != 1 {
		t.Fatalf("DequeuePending = %+v, %v", listed, err)
	}
	newer := *listed[0]
	newer.Payload = `{"checklist_name":"` + session.ChecklistName + `","items":[{"key":"K","match":"[r] A","name":"[r] A — unit ✓","resolved":true}]}`
	if err := st.CoalesceSyncOp(ctx, newer); err != nil {
		t.Fatalf("CoalesceSyncOp failed: %v", err)
	}

	w := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1})
	if err := w.processOp(ctx, listed[0]); err != nil {
		t.Fatalf("processOp failed: %v", err)
	}
	items, _ := checklistItems(t, fake)
	expectItems(t, items, []clickup.ChecklistItem{{ID: "item-2", Name: "[r] A — unit ✓", Resolved: true}})

	// Claimed and finished: listing it again doesn't run it twice.
	if err := w.processOp(ctx, listed[0]); !errors.Is(err, errNotClaimed) {
		t.Errorf("processing a finished op again = %v, want errNotClaimed", err)
	}
}
