package sync

import (
	"context"
	"encoding/json"
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
	if f.reject != "" && strings.HasPrefix(req, f.reject) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"err": "rejected"}`))
		return
	}
	var body struct {
		Name     string `json:"name"`
		Resolved *bool  `json:"resolved"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
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
		cl.Items = append(cl.Items, clickup.ChecklistItem{ID: f.id("item"), Name: body.Name})
		f.write(w, map[string]any{"checklist": cl})
	case r.Method == "PUT" && len(parts) == 4 && parts[0] == "checklist" && parts[2] == "checklist_item":
		cl := f.checklist(parts[1])
		for i := range cl.Items {
			if cl.Items[i].ID == parts[3] {
				cl.Items[i].Name = body.Name
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
	return session.ChecklistItem{Key: key, Match: match, Name: "~~" + match + "~~", Resolved: true, Void: true}
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

func TestWorker_SyncChecklist_StrikesVoids(t *testing.T) {
	t.Parallel()
	const task = "task-cl-void"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task, clickup.Checklist{ID: "cl-1", Name: session.ChecklistName, Items: []clickup.ChecklistItem{
		{ID: "i-story", Name: "[story] Stories for A — waived", Resolved: true},
		{ID: "i-test", Name: "[test] Test for A"},
	}})

	enqueueChecklist(t, st, task, voidItem("r/story/A", "[story] Stories for A"), voidItem("r/test/A", "[test] Test for A"))
	drain(t, st, client)

	cls, _ := fake.state()
	want := []clickup.ChecklistItem{
		{ID: "i-story", Name: "~~[story] Stories for A~~", Resolved: true},
		{ID: "i-test", Name: "~~[test] Test for A~~", Resolved: true},
	}
	if len(cls[0].Items) != len(want) {
		t.Fatalf("void items are renamed, never deleted: %+v", cls[0].Items)
	}
	for i := range want {
		if cls[0].Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, cls[0].Items[i], want[i])
		}
	}

	// Struck items still match, so a re-run changes nothing.
	enqueueChecklist(t, st, task, voidItem("r/story/A", "[story] Stories for A"), voidItem("r/test/A", "[test] Test for A"))
	drain(t, st, client)
	if _, reqs := fake.state(); len(reqs) != 1 {
		t.Errorf("re-run requests = %v", reqs)
	}

	// An obligation found again comes back unstruck.
	enqueueChecklist(t, st, task, item("r/test/A", "[test] Test for A", "", false))
	drain(t, st, client)
	cls, _ = fake.state()
	if it := cls[0].Items[1]; it.Name != "[test] Test for A" || it.Resolved {
		t.Errorf("revived item = %+v", it)
	}
}

func TestWorker_SyncChecklist_CoalescesToNewest(t *testing.T) {
	t.Parallel()
	const task = "task-cl-coalesce"
	st := newChecklistStore(t, task)
	fake, client := newFakeChecklists(t, task)

	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
	enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "structural ✓", true))
	if n := drain(t, st, client); n != 2 {
		t.Fatalf("expected both ops done (one skipped), got %d", n)
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
}

func TestWorker_SyncChecklist_ErrorClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a 400 to an item write is permanent", func(t *testing.T) {
		t.Parallel()
		const task = "task-cl-write-400"
		st := newChecklistStore(t, task)
		fake, client := newFakeChecklists(t, task)
		fake.reject = "POST /checklist/"
		enqueueChecklist(t, st, task, item("r/story/A", "[story] Stories for A", "", false))
		drain(t, st, client)
		st.SetRetryBackoffBase(time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		if ops, err := st.ListRetryable(ctx); err != nil || len(ops) != 0 {
			t.Errorf("expected no retryable ops, got %d (err: %v)", len(ops), err)
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
