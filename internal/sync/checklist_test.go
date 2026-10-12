package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

// fakeChecklists is a ClickUp that keeps one task's checklists in memory and
// records every request as "METHOD path".
type fakeChecklists struct {
	t      *testing.T
	taskID string

	mu         gosync.Mutex
	checklists []clickup.Checklist
	requests   []string
	nextID     int
	// reject answers requests whose "METHOD path" has this prefix with a 400.
	reject string
	// rejectName answers item writes whose name has this prefix with a 400.
	rejectName string
	// store is what ClickUp does to an item name it is sent (nil: keeps it).
	store func(name string) string
	// omitCreated leaves the new item out of the create item response.
	omitCreated bool
	// afterCreate runs, under the lock, after each item is created.
	afterCreate func(f *fakeChecklists)
}

func newFakeChecklists(t *testing.T, taskID string, existing ...clickup.Checklist) (*fakeChecklists, *clickup.Client) {
	t.Helper()
	f := &fakeChecklists{t: t, taskID: taskID, checklists: existing}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, clickup.New("my-key").WithBaseURL(server.URL)
}

func (f *fakeChecklists) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, req)
	var body struct {
		Name     string `json:"name"`
		Resolved *bool  `json:"resolved"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if (f.reject != "" && strings.HasPrefix(req, f.reject)) ||
		(f.rejectName != "" && strings.Contains(req, "/checklist_item") && strings.HasPrefix(body.Name, f.rejectName)) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"err": "rejected"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	switch {
	case r.Method == "GET" && req == "GET /task/"+f.taskID:
		f.write(w, map[string]any{"id": f.taskID, "checklists": f.checklists})
	case r.Method == "POST" && req == "POST /task/"+f.taskID+"/checklist":
		f.checklists = append(f.checklists, clickup.Checklist{ID: f.id("cl"), Name: body.Name, Items: []clickup.ChecklistItem{}})
		f.write(w, map[string]any{"checklist": f.checklists[len(f.checklists)-1]})
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "checklist" && parts[2] == "checklist_item":
		cl := f.checklist(parts[1])
		resp := *cl
		resp.Items = append([]clickup.ChecklistItem(nil), cl.Items...)
		cl.Items = append(cl.Items, clickup.ChecklistItem{ID: f.id("item"), Name: f.storedName(body.Name)})
		if !f.omitCreated {
			resp = *cl
		}
		if f.afterCreate != nil {
			f.afterCreate(f)
		}
		f.write(w, map[string]any{"checklist": resp})
	case r.Method == "PUT" && len(parts) == 4 && parts[0] == "checklist" && parts[2] == "checklist_item":
		cl := f.checklist(parts[1])
		for i := range cl.Items {
			if cl.Items[i].ID == parts[3] {
				cl.Items[i].Name = f.storedName(body.Name)
				if body.Resolved != nil {
					cl.Items[i].Resolved = *body.Resolved
				}
			}
		}
		f.write(w, map[string]any{"checklist": cl})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeChecklists) storedName(name string) string {
	if f.store == nil {
		return name
	}
	return f.store(name)
}

func (f *fakeChecklists) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeChecklists) checklist(id string) *clickup.Checklist {
	for i := range f.checklists {
		if f.checklists[i].ID == id {
			return &f.checklists[i]
		}
	}
	f.t.Fatalf("no checklist %s", id)
	return nil
}

func (f *fakeChecklists) write(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encoding response: %v", err)
	}
}

// state returns the task's checklists and the requests made since the last
// call.
func (f *fakeChecklists) state() ([]clickup.Checklist, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reqs := f.requests
	f.requests = nil
	cls := make([]clickup.Checklist, len(f.checklists))
	for i, cl := range f.checklists {
		cls[i] = cl
		cls[i].Items = append([]clickup.ChecklistItem(nil), cl.Items...)
	}
	return cls, reqs
}

func newChecklistStore(t *testing.T, taskID string) store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := &session.Session{ID: "sess-" + taskID, TaskID: taskID, ProjectPath: "/tmp", Status: session.StatusActive, StartedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	return st
}

func enqueueChecklist(t *testing.T, st store.Store, taskID string, items ...session.ChecklistItem) {
	t.Helper()
	b, _ := json.Marshal(session.ChecklistPayload{ChecklistName: session.ChecklistName, Items: items})
	op := session.SyncOp{Operation: session.OpSyncChecklist, TaskID: taskID, Payload: string(b), MaxRetries: 3, CreatedAt: time.Now()}
	if err := st.EnqueueSync(context.Background(), op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}
}

func item(key, match, suffix string, resolved bool) session.ChecklistItem {
	name := match
	if suffix != "" {
		name += " — " + suffix
	}
	return session.ChecklistItem{Key: key, Match: match, Name: name, Resolved: resolved}
}

func voidItem(key, match string) session.ChecklistItem {
	return session.ChecklistItem{Key: key, Match: match, Name: match + " — void", Resolved: true, Void: true}
}

func drain(t *testing.T, st store.Store, client *clickup.Client) int {
	t.Helper()
	n, err := NewWorker(st, client, &config.SyncConfig{MaxRetries: 3, RateLimitMS: 1}).DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce failed: %v", err)
	}
	return n
}

func itemNames(cl clickup.Checklist) map[string]bool {
	out := map[string]bool{}
	for _, it := range cl.Items {
		out[it.Name] = it.Resolved
	}
	return out
}

func countPrefix(reqs []string, prefix string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func TestWorker_SyncChecklist_CreatesChecklistAndItems(t *testing.T) {
	t.Parallel()
	const task = "task-cl-create"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task,
		item("r/story/A", "[story] Stories for A", "", false),
		item("r/test/A", "[test] Test for A", "structural ✓", true),
		voidItem("r/e2e/A", "[e2e] E2E for A"),
	)
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected 1 processed op, got %d", n)
	}

	cls, reqs := fake.state()
	if len(cls) != 1 || cls[0].Name != session.ChecklistName {
		t.Fatalf("checklists = %+v", cls)
	}
	want := map[string]bool{"[story] Stories for A": false, "[test] Test for A — structural ✓": true}
	got := itemNames(cls[0])
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v (a void item is never created)", got, want)
	}
	for name, resolved := range want {
		if r, ok := got[name]; !ok || r != resolved {
			t.Errorf("item %q: resolved=%v present=%v, want resolved=%v", name, r, ok, resolved)
		}
	}
	if countPrefix(reqs, "POST /task/") != 1 || countPrefix(reqs, "POST /checklist/") != 2 || countPrefix(reqs, "PUT ") != 1 {
		t.Errorf("requests = %v", reqs)
	}
}

func TestWorker_SyncChecklist_UpdatesByStablePart(t *testing.T) {
	t.Parallel()
	const task = "task-cl-update"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task, clickup.Checklist{ID: "cl-1", Name: session.ChecklistName, Items: []clickup.ChecklistItem{
		{ID: "i-story", Name: "[story] Stories for A"},
		{ID: "i-e2e", Name: "[e2e] E2E for A — attested", Resolved: true},
		{ID: "i-mine", Name: "Someone's own item"},
	}})

	// Stories got written, and the e2e attestation's checks now pass.
	enqueueChecklist(t, st, task,
		item("r/story/A", "[story] Stories for A", "structural ✓", true),
		item("r/e2e/A", "[e2e] E2E for A", "attested, structural ✓", true),
	)
	drain(t, st, client)

	cls, reqs := fake.state()
	got := cls[0].Items
	if len(got) != 3 {
		t.Fatalf("an item whose status text changed must be updated, not duplicated: %+v", got)
	}
	want := []clickup.ChecklistItem{
		{ID: "i-story", Name: "[story] Stories for A — structural ✓", Resolved: true},
		{ID: "i-e2e", Name: "[e2e] E2E for A — attested, structural ✓", Resolved: true},
		{ID: "i-mine", Name: "Someone's own item"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if countPrefix(reqs, "POST ") != 0 || countPrefix(reqs, "PUT ") != 2 {
		t.Errorf("requests = %v", reqs)
	}
}

func TestWorker_SyncChecklist_IdempotentRerun(t *testing.T) {
	t.Parallel()
	const task = "task-cl-rerun"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)
	items := []session.ChecklistItem{
		item("r/story/A", "[story] Stories for A", "", false),
		item("r/test/A", "[test] Test for A", "structural ✓", true),
		// Two obligations with the same text (different subjects) get one
		// item each.
		item("r/story/x/B", "[story] Stories for B", "", false),
		item("r/story/y/B", "[story] Stories for B", "", false),
	}

	enqueueChecklist(t, st, task, items...)
	drain(t, st, client)
	first, _ := fake.state()

	enqueueChecklist(t, st, task, items...)
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected the re-run to succeed, got %d processed", n)
	}
	second, reqs := fake.state()
	if len(reqs) != 1 || reqs[0] != "GET /task/"+task {
		t.Errorf("a re-run of the same state must only read the task: %v", reqs)
	}
	if len(second) != 1 || len(second[0].Items) != len(first[0].Items) || len(second[0].Items) != 4 {
		t.Errorf("checklists after re-run = %+v", second)
	}
}

func TestWorker_SyncChecklist_MarksVoids(t *testing.T) {
	t.Parallel()
	const task = "task-cl-void"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task, clickup.Checklist{ID: "cl-1", Name: session.ChecklistName, Items: []clickup.ChecklistItem{
		{ID: "i-story", Name: "[story] Stories for A — waived", Resolved: true},
		{ID: "i-test", Name: "[test] Test for A"},
		// Struck through: the void form of earlier builds.
		{ID: "i-e2e", Name: "~~[e2e] E2E for A~~", Resolved: true},
	}})
	voids := []session.ChecklistItem{
		voidItem("r/story/A", "[story] Stories for A"),
		voidItem("r/test/A", "[test] Test for A"),
		voidItem("r/e2e/A", "[e2e] E2E for A"),
	}

	enqueueChecklist(t, st, task, voids...)
	drain(t, st, client)

	cls, _ := fake.state()
	want := []clickup.ChecklistItem{
		{ID: "i-story", Name: "[story] Stories for A — void", Resolved: true},
		{ID: "i-test", Name: "[test] Test for A — void", Resolved: true},
		{ID: "i-e2e", Name: "[e2e] E2E for A — void", Resolved: true},
	}
	if len(cls[0].Items) != len(want) {
		t.Fatalf("void items are renamed, never deleted: %+v", cls[0].Items)
	}
	for i := range want {
		if cls[0].Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, cls[0].Items[i], want[i])
		}
	}

	// Void items still match, so a re-run changes nothing.
	enqueueChecklist(t, st, task, voids...)
	drain(t, st, client)
	if _, reqs := fake.state(); len(reqs) != 1 {
		t.Errorf("re-run requests = %v", reqs)
	}

	// An obligation found again comes back.
	enqueueChecklist(t, st, task, item("r/test/A", "[test] Test for A", "", false))
	drain(t, st, client)
	cls, _ = fake.state()
	if it := cls[0].Items[1]; it.Name != "[test] Test for A" || it.Resolved {
		t.Errorf("revived item = %+v", it)
	}
}

// Two ops queued side by side (as when the newer is queued while the older
// is in flight, so CoalesceSyncOp can't merge them): only the newer applies.
func TestWorker_SyncChecklist_SkipsOlderOp(t *testing.T) {
	t.Parallel()
	const task = "task-cl-coalesce"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "structural ✓", true))
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected one op synced and the skipped one not counted, got %d", n)
	}

	cls, reqs := fake.state()
	if countPrefix(reqs, "GET ") != 1 {
		t.Errorf("the older op must be skipped without calling ClickUp: %v", reqs)
	}
	if got := itemNames(cls[0]); len(got) != 1 || !got["[story] Stories for A — structural ✓"] {
		t.Errorf("items = %v, want only the newest state", got)
	}
	if n, _ := st.CountPendingByTaskID(context.Background(), task); n != 0 {
		t.Errorf("pending = %d", n)
	}
	if got := checklistOpStatuses(t, st, task); len(got) != 2 || got[0] != "SKIPPED" || got[1] != "DONE" {
		t.Errorf("op statuses = %v, want the older op SKIPPED and the newer DONE", got)
	}
}

// checklistOpStatuses lists the task's SYNC_CHECKLIST op statuses, oldest first.
func checklistOpStatuses(t *testing.T, st store.Store, taskID string) []string {
	t.Helper()
	ops, err := st.ListSyncOps(context.Background(), taskID, session.OpSyncChecklist, 0, 50)
	if err != nil {
		t.Fatalf("ListSyncOps failed: %v", err)
	}
	out := make([]string, len(ops))
	for i, op := range ops {
		out[len(ops)-1-i] = op.Status
	}
	return out
}

// An op skipped for a newer one applied nothing, so when that newer one fails
// for good, a retried older op must still apply its state instead of being
// skipped too and leaving the checklist stale.
func TestWorker_SyncChecklist_RetryNotHeldBackBySkippedOp(t *testing.T) {
	t.Parallel()
	const task = "task-cl-skipped-chain"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	// op#1 fails transiently and waits out its backoff.
	fake.reject = "GET /task/"
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	drain(t, st, client)

	// op#2 is skipped for op#3, whose checklist create is then rejected for
	// good.
	fake.mu.Lock()
	fake.reject = "POST /task/"
	fake.mu.Unlock()
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "structural ✓", true))
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "attested", true))
	drain(t, st, client)
	if got := checklistOpStatuses(t, st, task); len(got) != 3 || got[1] != "SKIPPED" || got[2] != "FAILED" {
		t.Fatalf("op statuses = %v, want op#2 SKIPPED and op#3 FAILED", got)
	}

	// op#1's retry is the only op left that can apply a state.
	fake.mu.Lock()
	fake.reject = ""
	fake.mu.Unlock()
	fake.state()
	st.SetRetryBackoffBase(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected op#1's retry to be processed, got %d", n)
	}
	cls, _ := fake.state()
	if len(cls) != 1 {
		t.Fatalf("checklists = %+v", cls)
	}
	got := itemNames(cls[0])
	if resolved, ok := got["[story] Stories for A"]; len(got) != 1 || !ok || resolved {
		t.Errorf("items = %v, want op#1's open item", got)
	}
	if got := checklistOpStatuses(t, st, task); got[0] != "DONE" {
		t.Errorf("op#1 status = %s, want DONE", got[0])
	}
}

func TestWorker_SyncChecklist_ErrorClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a 400 to one item write skips only that item, for good", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-write-400"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		fake.rejectName = "[story] Stories for B"
		enqueueChecklist(t, st, task,
			item("r/story/A", "[story] Stories for A", "", false),
			item("r/story/B", "[story] Stories for B", "", false),
			item("r/story/C", "[story] Stories for C", "structural ✓", true),
		)
		if n := drain(t, st, client); n != 1 {
			t.Fatalf("expected the op to count as synced, got %d", n)
		}
		cls, _ := fake.state()
		if got := itemNames(cls[0]); len(got) != 2 || got["[story] Stories for A"] || !got["[story] Stories for C — structural ✓"] {
			t.Errorf("items = %v, want A and C, B rejected", got)
		}
		st.SetRetryBackoffBase(time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
			t.Errorf("expected no retryable ops, got %d (err: %v)", len(ops), err)
		}
		ops, err := st.ListSyncOps(ctx, task, session.OpSyncChecklist, 0, 1)
		if err != nil || len(ops) != 1 || ops[0].Status != "DONE" || !strings.Contains(ops[0].ErrorMsg, "Stories for B") {
			t.Errorf("op = %+v (err: %v), want DONE noting the rejected item", ops, err)
		}
	})

	t.Run("a 400 to an item update skips only that item", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-update-400"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		enqueueChecklist(t, st, task,
			item("r/story/A", "[story] Stories for A", "", false),
			item("r/story/B", "[story] Stories for B", "", false),
		)
		drain(t, st, client)
		fake.mu.Lock()
		fake.rejectName = "[story] Stories for A"
		fake.mu.Unlock()
		enqueueChecklist(t, st, task,
			item("r/story/A", "[story] Stories for A", "structural ✓", true),
			item("r/story/B", "[story] Stories for B", "structural ✓", true),
		)
		drain(t, st, client)
		cls, _ := fake.state()
		if got := itemNames(cls[0]); len(got) != 2 || got["[story] Stories for A"] || !got["[story] Stories for B — structural ✓"] {
			t.Errorf("items = %v, want A unchanged and B resolved", got)
		}
	})

	t.Run("a 400 to the checklist lookup is retried", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-lookup-400"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		fake.reject = "GET /task/"
		enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
		drain(t, st, client)
		st.SetRetryBackoffBase(time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		ops, err := st.ListRetryable(ctx)
		if err != nil || len(ops) != 1 || ops[0].Retries != 1 {
			t.Fatalf("expected the op to be retryable after one attempt, got %+v (err: %v)", ops, err)
		}

		// Once ClickUp accepts it, the retry completes the sync.
		fake.mu.Lock()
		fake.reject = ""
		fake.mu.Unlock()
		drain(t, st, client)
		if cls, _ := fake.state(); len(cls) != 1 || len(cls[0].Items) != 1 {
			t.Errorf("checklists after retry = %+v", cls)
		}
	})

	t.Run("an invalid payload is permanent", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-bad-payload"
		st := newChecklistStore(t, task)
		_, client := newFakeChecklists(t, task)
		op := session.SyncOp{Operation: session.OpSyncChecklist, TaskID: task, Payload: `{"items": "nope"}`, MaxRetries: 3, CreatedAt: time.Now()}
		if err := st.EnqueueSync(ctx, op); err != nil {
			t.Fatalf("EnqueueSync failed: %v", err)
		}
		drain(t, st, client)
		if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
			t.Errorf("expected no retryable ops, got %d (err: %v)", len(ops), err)
		}
	})
}

func TestWorker_SyncChecklist_MatchesByEarlierMatchOfKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const task = "task-cl-describe"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	// An earlier op whose payload can't be parsed is ignored.
	bad := session.SyncOp{Operation: session.OpSyncChecklist, TaskID: task, Payload: `{"items": "nope"}`, MaxRetries: 3, CreatedAt: time.Now()}
	if err := st.EnqueueSync(ctx, bad); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}
	enqueueChecklist(t, st, task,
		item("r/story/A", "[story] Stories for A", "", false),
		item("r/test/A", "[test] Test for A", "", false),
	)
	drain(t, st, client)
	fake.mu.Lock()
	fake.checklists[0].Items = append(fake.checklists[0].Items, clickup.ChecklistItem{ID: "i-new-test", Name: "[test] Test for module A"})
	fake.mu.Unlock()
	fake.state()

	// Both describes were edited in the manifest.
	enqueueChecklist(t, st, task,
		item("r/story/A", "[story] Stories for module A", "structural ✓", true),
		item("r/test/A", "[test] Test for module A", "", false),
	)
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected 1 processed op, got %d", n)
	}

	cls, reqs := fake.state()
	want := []clickup.ChecklistItem{
		// Renamed: found by the Match its Key had in the earlier op.
		{ID: "item-2", Name: "[story] Stories for module A — structural ✓", Resolved: true},
		// The Key's own item wins over one that only matches its current
		// Match (added by someone else), which is left alone.
		{ID: "item-3", Name: "[test] Test for module A"},
		{ID: "i-new-test", Name: "[test] Test for module A"},
	}
	got := cls[0].Items
	if len(got) != len(want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if countPrefix(reqs, "POST ") != 0 || countPrefix(reqs, "PUT ") != 2 {
		t.Errorf("requests = %v", reqs)
	}
}

// listFailingStore fails ListSyncHistory.
type listFailingStore struct{ store.Store }

func (listFailingStore) ListSyncHistory(context.Context, string, string, int64, int) ([]*session.SyncOp, error) {
	return nil, errors.New("disk on fire")
}

func TestWorker_SyncChecklist_EarlierOpsUnreadable_StillSyncs(t *testing.T) {
	t.Parallel()
	const task = "task-cl-describe-noops"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	drain(t, st, client)
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for module A", "", false))
	if n := drain(t, listFailingStore{st}, client); n != 1 {
		t.Fatalf("expected the sync to go ahead without earlier ops, got %d processed", n)
	}
	if cls, _ := fake.state(); len(cls[0].Items) != 2 {
		t.Errorf("without earlier ops the item can only be created: %+v", cls[0].Items)
	}
}

func TestWorker_SyncChecklist_EarlierOpsPruned_StillRenames(t *testing.T) {
	t.Parallel()
	const task = "task-cl-describe-pruned"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	drain(t, st, client)
	// Prune every finished op from the queue: the history the rename needs
	// is kept apart from it.
	if n, err := st.PruneSyncQueue(context.Background(), -time.Hour); err != nil || n != 1 {
		t.Fatalf("PruneSyncQueue = %d, %v; want the applied op pruned", n, err)
	}
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for module A", "", false))
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected the op to sync, got %d processed", n)
	}
	if cls, _ := fake.state(); len(cls[0].Items) != 1 || cls[0].Items[0].Name != "[story] Stories for module A" {
		t.Errorf("items = %+v, want the one item renamed", cls[0].Items)
	}
}

func TestWorker_SyncChecklist_ClickUpNormalizesNames(t *testing.T) {
	t.Parallel()
	// A name queued by an older taskflow, before names were normalized.
	unnormalized := item("r/story/A", "[story] Stories  for A ", "structural ✓", true)
	const stored = "[story] Stories for A — structural ✓"

	t.Run("the created item is found and resolved", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-normalize"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		fake.store = session.NormalizeChecklistName

		enqueueChecklist(t, st, task, unnormalized)
		if n := drain(t, st, client); n != 1 {
			t.Fatalf("expected 1 processed op, got %d", n)
		}
		cls, _ := fake.state()
		if got := itemNames(cls[0]); len(got) != 1 || !got[stored] {
			t.Fatalf("items = %v, want only %q resolved", got, stored)
		}

		// The normalized name isn't "renamed" back on every sync.
		enqueueChecklist(t, st, task, unnormalized)
		drain(t, st, client)
		if _, reqs := fake.state(); len(reqs) != 1 {
			t.Errorf("re-run requests = %v", reqs)
		}
	})

	t.Run("an item missing from the create response is read back", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-normalize-retry"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		fake.store = session.NormalizeChecklistName
		fake.omitCreated = true

		enqueueChecklist(t, st, task, unnormalized)
		if n := drain(t, st, client); n != 1 {
			t.Fatalf("expected 1 processed op, got %d", n)
		}
		cls, reqs := fake.state()
		if got := itemNames(cls[0]); len(got) != 1 || !got[stored] {
			t.Errorf("items = %v, want only %q resolved", got, stored)
		}
		if countPrefix(reqs, "POST /checklist/") != 1 {
			t.Errorf("the item must be created once: %v", reqs)
		}
	})
}

func TestWorker_SyncChecklist_ResolvesCreatedItemsByID(t *testing.T) {
	t.Parallel()
	const task = "task-cl-created-ids"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task, clickup.Checklist{ID: "cl-1", Name: session.ChecklistName, Items: []clickup.ChecklistItem{
		{ID: "i-mine", Name: "Someone's own item"},
	}})
	// Truncated names can't be found by name: only the IDs each create adds
	// tell the created items apart, the unresolved ones included.
	fake.store = func(name string) string { return name[:20] }

	enqueueChecklist(t, st, task,
		item("r/story/A", "[story] Stories for screen A", "", false),
		item("r/story/B", "[story] Stories for screen B", "structural ✓", true),
		item("r/story/C", "[story] Stories for screen C", "structural ✓", true),
	)
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected 1 processed op, got %d", n)
	}

	cls, _ := fake.state()
	got := cls[0].Items
	want := []struct {
		id       string
		resolved bool
	}{{"i-mine", false}, {"item-1", false}, {"item-2", true}, {"item-3", true}}
	if len(got) != len(want) {
		t.Fatalf("items = %+v", got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Resolved != w.resolved {
			t.Errorf("item %d = %+v, want ID %s resolved=%v", i, got[i], w.id, w.resolved)
		}
	}
}

func TestWorker_SyncChecklist_StaleRetryAfterNewerDone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const task = "task-cl-stale-retry"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	// op#1 (item open) fails transiently.
	fake.reject = "GET /task/"
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	drain(t, st, client)

	// op#2 (item resolved) runs before op#1's backoff is up.
	fake.mu.Lock()
	fake.reject = ""
	fake.mu.Unlock()
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "structural ✓", true))
	if n := drain(t, st, client); n != 1 {
		t.Fatalf("expected op#2 to be processed alone, got %d", n)
	}
	fake.state()

	// op#1's retry is skipped, and not counted as synced: it must not undo
	// op#2.
	st.SetRetryBackoffBase(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if n := drain(t, st, client); n != 0 {
		t.Fatalf("expected op#1's retry to be skipped, got %d synced", n)
	}
	if got := checklistOpStatuses(t, st, task); got[0] != "SKIPPED" {
		t.Errorf("op#1 status = %s, want SKIPPED", got[0])
	}
	cls, reqs := fake.state()
	if len(reqs) != 0 {
		t.Errorf("the stale op must be skipped without calling ClickUp: %v", reqs)
	}
	if got := itemNames(cls[0]); len(got) != 1 || !got["[story] Stories for A — structural ✓"] {
		t.Errorf("items = %v, want the newer, resolved state", got)
	}
	if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
		t.Errorf("expected no retryable ops, got %d (err: %v)", len(ops), err)
	}
}
