package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeEvaluator struct {
	baseSHA string
	eval    *Evaluation // nil = no manifest
	calls   int
	// plan resolves a plan item to its subject and the findings for it;
	// nil rejects every item.
	plan func(item PlanItem) (PlannedSubject, []CompletenessFinding, error)
	// err fails every evaluation.
	err error
}

func (f *fakeEvaluator) BaseSHA(projectPath string) (string, error) {
	return f.baseSHA, nil
}

func (f *fakeEvaluator) Evaluate(ctx context.Context, sess *Session, stored []*Obligation) (*Evaluation, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.eval == nil {
		return nil, nil
	}
	c := *f.eval
	return &c, nil
}

func (f *fakeEvaluator) Plan(ctx context.Context, sess *Session, stored []*Obligation, items []PlanItem) (*Evaluation, error) {
	ev, err := f.Evaluate(ctx, sess, stored)
	if ev == nil || err != nil {
		return ev, err
	}
	ev.Findings = slices.Clone(ev.Findings)
	for _, item := range items {
		if f.plan == nil {
			return nil, fmt.Errorf("%w: rule %q is not plannable", ErrInvalidPlan, item.RuleID)
		}
		ps, findings, err := f.plan(item)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPlan, err)
		}
		ev.Planned = append(ev.Planned, ps)
		ev.Findings = append(ev.Findings, findings...)
	}
	return ev, nil
}

func finding(rule, req, subject string, satisfied bool) CompletenessFinding {
	return CompletenessFinding{
		RuleID: rule, RequirementID: req, Subject: subject,
		Severity: "required", Describe: req + " for " + subject,
		Expected: []string{subject + "." + req}, Satisfied: satisfied,
	}
}

func evalWith(findings ...CompletenessFinding) *Evaluation {
	test := "pnpm test"
	return &Evaluation{
		Manifest: ".agents/completeness.yaml",
		Stacks:   []StackInfo{{ID: "web", Root: ".", Commands: map[string]*string{"test": &test}}},
		Findings: findings,
	}
}

func stored(rule, req, subject string, status ObligationStatus) *Obligation {
	return &Obligation{ID: rule + "/" + req + "/" + subject, SessionID: "s", RuleID: rule, RequirementID: req,
		Subject: subject, Severity: "required", Describe: req + " for " + subject, Expected: []string{subject + "." + req}, Status: status}
}

func TestMergeObligations(t *testing.T) {
	now := time.Now()

	t.Run("new keys are inserted open or satisfied", func(t *testing.T) {
		mr := mergeObligations("s", nil, evalWith(finding("r", "story", "A", false), finding("r", "test", "A", true)), now)
		if len(mr.upserts) != 2 || len(mr.obligations) != 2 {
			t.Fatalf("upserts=%d obligations=%d", len(mr.upserts), len(mr.obligations))
		}
		open, sat := mr.upserts[0], mr.upserts[1]
		if open.Status != ObligationOpen || !mr.newlyOpen[open.ID] || open.SatisfiedAt != nil {
			t.Errorf("open insert = %+v", open)
		}
		if sat.Status != ObligationSatisfied || sat.SatisfiedAt == nil || mr.newlyOpen[sat.ID] {
			t.Errorf("satisfied insert = %+v", sat)
		}
		if !open.FirstSeenAt.Equal(now) || open.ID == "" || open.ID == sat.ID {
			t.Errorf("insert identity/first_seen wrong: %+v", open)
		}
	})

	t.Run("open and satisfied are recomputed", func(t *testing.T) {
		toSat, toOpen := stored("r", "story", "A", ObligationOpen), stored("r", "test", "A", ObligationSatisfied)
		mr := mergeObligations("s", []*Obligation{toSat, toOpen}, evalWith(finding("r", "story", "A", true), finding("r", "test", "A", false)), now)
		if toSat.Status != ObligationSatisfied || toSat.SatisfiedAt == nil || len(mr.newlySatisfied) != 1 || mr.newlySatisfied[0] != toSat {
			t.Errorf("open → satisfied: %+v, newlySatisfied=%v", toSat, mr.newlySatisfied)
		}
		if toOpen.Status != ObligationOpen || toOpen.SatisfiedAt != nil || !mr.newlyOpen[toOpen.ID] {
			t.Errorf("satisfied → open: %+v", toOpen)
		}
		if len(mr.upserts) != 2 {
			t.Errorf("both transitions must be written, got %d", len(mr.upserts))
		}
	})

	t.Run("waived and attested are sticky", func(t *testing.T) {
		waived, attested := stored("r", "story", "A", ObligationWaived), stored("r", "e2e", "A", ObligationAttested)
		gone := stored("r", "story", "B", ObligationWaived)
		mergeObligations("s", []*Obligation{waived, attested, gone}, evalWith(finding("r", "story", "A", false), finding("r", "e2e", "A", true)), now)
		if waived.Status != ObligationWaived || attested.Status != ObligationAttested {
			t.Errorf("sticky statuses changed: %s, %s", waived.Status, attested.Status)
		}
		if gone.Status != ObligationWaived {
			t.Errorf("a waiver must survive its key disappearing, got %s", gone.Status)
		}
	})

	t.Run("keys no longer found are voided", func(t *testing.T) {
		gone, kept := stored("r", "story", "Reverted", ObligationOpen), stored("r", "story", "A", ObligationOpen)
		planned := stored("r", "story", "Planned", ObligationOpen)
		planned.Planned = true
		inherited := stored("r", "story", "Inherited", ObligationOpen)
		inherited.InheritedFrom = "older"
		mr := mergeObligations("s", []*Obligation{gone, kept, planned, inherited}, evalWith(finding("r", "story", "A", false)), now)
		if gone.Status != ObligationVoid || len(mr.voided) != 1 || mr.voided[0] != gone {
			t.Errorf("reverted file should void its obligation: %+v voided=%v", gone, mr.voided)
		}
		if planned.Status != ObligationOpen || inherited.Status != ObligationOpen {
			t.Errorf("planned/inherited obligations must not be voided")
		}
		if len(mr.upserts) != 0 {
			t.Errorf("an unchanged obligation must not be rewritten, got %d upserts", len(mr.upserts))
		}
	})

	t.Run("a partial evaluation never voids", func(t *testing.T) {
		o := stored("r", "story", "A", ObligationSatisfied)
		ev := evalWith()
		ev.Partial = true
		mr := mergeObligations("s", []*Obligation{o}, ev, now)
		if o.Status != ObligationSatisfied || len(mr.voided) != 0 {
			t.Errorf("partial evaluation voided %+v", o)
		}
	})

	t.Run("a void obligation found again comes back", func(t *testing.T) {
		o := stored("r", "story", "A", ObligationVoid)
		mr := mergeObligations("s", []*Obligation{o}, evalWith(finding("r", "story", "A", false)), now)
		if o.Status != ObligationOpen || !mr.newlyOpen[o.ID] || len(mr.upserts) != 1 {
			t.Errorf("revived = %+v", o)
		}
	})

	t.Run("changed text is refreshed", func(t *testing.T) {
		o := stored("r", "story", "A", ObligationWaived)
		f := finding("r", "story", "A", false)
		f.Describe = "new wording"
		mr := mergeObligations("s", []*Obligation{o}, evalWith(f), now)
		if o.Describe != "new wording" || o.Status != ObligationWaived || len(mr.upserts) != 1 {
			t.Errorf("refresh = %+v upserts=%d", o, len(mr.upserts))
		}
	})
}

func newCompletenessService(t *testing.T, ev *fakeEvaluator) (*Service, *mockStore, string) {
	t.Helper()
	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "abc123"}}
	return NewService(store, git, nil).WithCompleteness(ev), store, t.TempDir()
}

// comments returns the text of every queued POST_COMMENT op, in order.
func (m *mockStore) comments(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, op := range m.syncOps {
		if op.Operation != "POST_COMMENT" {
			continue
		}
		var payload struct {
			CommentText string `json:"comment_text"`
		}
		if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
			t.Fatalf("invalid POST_COMMENT payload: %v", err)
		}
		out = append(out, payload.CommentText)
	}
	return out
}

func TestService_BaseSHA(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base-1"}
	srv, store, dir := newCompletenessService(t, ev)
	ctx := context.Background()

	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Session.BaseSHA != "base-1" || store.sessions[res.Session.ID].BaseSHA != "base-1" {
		t.Fatalf("base sha not recorded: %+v", res.Session)
	}

	// Commits made while paused must not move the base on resume.
	if _, err := srv.Stop(ctx, res.Session.ID, "", "PAUSED", false, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ev.baseSHA = "base-2"
	resumed, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Session.ID != res.Session.ID || resumed.Session.BaseSHA != "base-1" {
		t.Errorf("resume changed the base: %+v", resumed.Session)
	}

	// A restart after completion is a new session with its own base.
	if _, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	restarted, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.Session.ID == res.Session.ID || restarted.Session.BaseSHA != "base-2" {
		t.Errorf("restart = %+v", restarted.Session)
	}
}

func TestService_CheckpointCompleteness(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("react-screen", "story", "Page.tsx", false), finding("react-screen", "unit-test", "Page.tsx", true))}
	srv, store, dir := newCompletenessService(t, ev)
	ctx := context.Background()

	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cc := res.AgenticContract.Completeness
	if cc == nil || cc.Manifest != ".agents/completeness.yaml" || len(cc.Stacks) != 1 || *cc.Stacks[0].Commands["test"] != "pnpm test" {
		t.Fatalf("contract completeness = %+v", cc)
	}
	sid := res.Session.ID
	if n := len(store.obligations[sid]); n != 2 {
		t.Fatalf("Start should record obligations, got %d", n)
	}
	// The contract shows what's open from the start.
	if len(cc.Open) != 1 || cc.Open[0].Describe != "story for Page.tsx" || cc.Open[0].Expected != "Page.tsx.story" || cc.Open[0].Rule != "react-screen" {
		t.Errorf("contract open = %+v", cc.Open)
	}
	if cc.Summary != (CompletenessSummary{Open: 1, Satisfied: 1}) || cc.Gate != GateReview || cc.Protocol == "" {
		t.Errorf("contract summary=%+v gate=%q protocol=%q", cc.Summary, cc.Gate, cc.Protocol)
	}

	// First checkpoint: the agent saw the open obligation in the contract.
	cp, err := srv.Checkpoint(ctx, sid, "built the page", []string{"Page.tsx"}, false)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c := cp.Completeness
	if c == nil || c.Summary != (CompletenessSummary{Open: 1, Satisfied: 1}) {
		t.Fatalf("summary = %+v", c)
	}
	if len(c.NewlyOpen) != 0 || !slices.Equal(c.StillOpen, []string{"story for Page.tsx"}) || len(c.NewlySatisfied) != 0 {
		t.Errorf("newly_open=%+v still_open=%v newly_satisfied=%v", c.NewlyOpen, c.StillOpen, c.NewlySatisfied)
	}

	comments := store.comments(t)
	if last := comments[len(comments)-1]; !strings.HasSuffix(last, "\n**Completeness:** 1/2 · open: story") {
		t.Errorf("checkpoint comment = %q", last)
	}

	// A requirement found mid-session is new to the agent.
	ev.eval = evalWith(finding("react-screen", "story", "Page.tsx", false), finding("react-screen", "unit-test", "Page.tsx", true), finding("react-screen", "e2e", "Page.tsx", false))
	cp, _ = srv.Checkpoint(ctx, sid, "routing", nil, false)
	if c := cp.Completeness; len(c.NewlyOpen) != 1 || c.NewlyOpen[0].Requirement != "e2e" || c.NewlyOpen[0].Expected != "Page.tsx.e2e" {
		t.Errorf("newly_open = %+v", c.NewlyOpen)
	}
	ev.eval = evalWith(finding("react-screen", "story", "Page.tsx", false), finding("react-screen", "unit-test", "Page.tsx", true))

	// Next checkpoint: same state, so it's a reminder.
	cp, _ = srv.Checkpoint(ctx, sid, "styling", nil, false)
	if c := cp.Completeness; len(c.NewlyOpen) != 0 || !slices.Equal(c.StillOpen, []string{"story for Page.tsx"}) {
		t.Errorf("second checkpoint = %+v", c)
	}
	for _, o := range store.obligations[sid] {
		want := 0
		switch {
		case o.RequirementID == "story":
			want = 4 // start + three checkpoints
		case o.RequirementID == "e2e":
			want = 1 // voided once its finding went away
		}
		if o.RemindedCount != want {
			t.Errorf("%s reminded %d times, want %d", o.RequirementID, o.RemindedCount, want)
		}
	}

	// The story lands.
	ev.eval = evalWith(finding("react-screen", "story", "Page.tsx", true), finding("react-screen", "unit-test", "Page.tsx", true))
	cp, _ = srv.Checkpoint(ctx, sid, "stories", nil, false)
	if c := cp.Completeness; !slices.Equal(c.NewlySatisfied, []string{"story for Page.tsx"}) || c.Summary.Open != 0 {
		t.Errorf("after story = %+v", c)
	}
	comments = store.comments(t)
	if last := comments[len(comments)-1]; !strings.HasSuffix(last, "**Completeness:** 2/2 · nothing open") {
		t.Errorf("checkpoint comment = %q", last)
	}

	// The checkpoint's own fields stay at the top level of the JSON.
	b, _ := json.Marshal(cp)
	var shape map[string]any
	_ = json.Unmarshal(b, &shape)
	for _, k := range []string{"id", "session_id", "summary", "files", "completeness"} {
		if _, ok := shape[k]; !ok {
			t.Errorf("checkpoint JSON missing %q: %s", k, b)
		}
	}
}

func TestService_NoManifestChangesNothing(t *testing.T) {
	for name, srvFor := range map[string]func(*mockStore) *Service{
		"nil evaluator": func(st *mockStore) *Service { return NewService(st, &mockGit{}, nil) },
		"no manifest": func(st *mockStore) *Service {
			return NewService(st, &mockGit{}, nil).WithCompleteness(&fakeEvaluator{baseSHA: "b"})
		},
		"nil via option": func(st *mockStore) *Service { return NewService(st, &mockGit{}, nil).WithCompleteness(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newMockStore()
			srv := srvFor(store)
			ctx := context.Background()
			res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.AgenticContract.Completeness != nil {
				t.Errorf("contract has completeness: %+v", res.AgenticContract.Completeness)
			}
			cp, err := srv.Checkpoint(ctx, res.Session.ID, "work", nil, false)
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			b, _ := json.Marshal(cp)
			if cp.Completeness != nil || strings.Contains(string(b), "completeness") {
				t.Errorf("checkpoint JSON mentions completeness: %s", b)
			}
			if _, err := srv.Stop(ctx, res.Session.ID, "done", "COMPLETED", false, false); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			for _, c := range store.comments(t) {
				if strings.Contains(c, "Completeness") {
					t.Errorf("comment mentions completeness: %q", c)
				}
			}
			report, err := srv.Completeness(ctx, res.Session.ID)
			if err != nil || report.Enabled || report.Obligations == nil {
				t.Errorf("report = %+v, %v", report, err)
			}
		})
	}
}

func TestService_InvalidManifest(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("r", "story", "A", false))}
	srv, store, dir := newCompletenessService(t, ev)
	ctx := context.Background()
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID

	ev.eval = &Evaluation{Manifest: ".agents/completeness.yaml", Error: "version: must be 1, got 2"}
	cp, err := srv.Checkpoint(ctx, sid, "work", nil, false)
	if err != nil {
		t.Fatalf("Checkpoint must succeed with a broken manifest: %v", err)
	}
	if cp.Completeness.Error != "version: must be 1, got 2" {
		t.Errorf("checkpoint error = %q", cp.Completeness.Error)
	}
	if o := store.obligations[sid][0]; o.Status != ObligationOpen {
		t.Errorf("a broken manifest must not void obligations, got %s", o.Status)
	}
	comments := store.comments(t)
	if last := comments[len(comments)-1]; !strings.Contains(last, "**Completeness:** manifest error: version: must be 1") {
		t.Errorf("comment = %q", last)
	}

	// A session started with a broken manifest still starts.
	restartSrv, _, dir2 := newCompletenessService(t, ev)
	res2, err := restartSrv.Start(ctx, "task-2", dir2, false)
	if err != nil {
		t.Fatalf("Start with broken manifest: %v", err)
	}
	if res2.AgenticContract.Completeness == nil || res2.AgenticContract.Completeness.Error == "" {
		t.Errorf("contract should carry the manifest error: %+v", res2.AgenticContract.Completeness)
	}
}

func TestService_StopReport(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("react-screen", "story", "Page.tsx", false), finding("react-screen", "unit-test", "Page.tsx", true))}
	srv, store, dir := newCompletenessService(t, ev)
	ctx := context.Background()
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev.eval.Findings[1].Levels = []string{"structural"}
	if _, err := srv.Stop(ctx, res.Session.ID, "wrapping up", "COMPLETED", false, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	comments := store.comments(t)
	if len(comments) != 2 {
		t.Fatalf("expected stop checkpoint + finished comments, got %d", len(comments))
	}
	if strings.Contains(comments[0], "Completeness") {
		t.Errorf("stop checkpoint comment repeats the finished comment's table: %q", comments[0])
	}
	finished := comments[1]
	for _, want := range []string{
		"## Session Finished",
		"### Completeness (1/2)",
		"| Requirement | Subject | Severity | Status | Verified | Evidence |",
		"| story for Page.tsx | `Page.tsx` | required | open | — | — |",
		"| unit-test for Page.tsx | `Page.tsx` | required | satisfied | structural ✓ | — |",
	} {
		if !strings.Contains(finished, want) {
			t.Errorf("finished comment missing %q:\n%s", want, finished)
		}
	}
	if strings.Index(finished, "story for") > strings.Index(finished, "unit-test for") {
		t.Errorf("open obligations should be listed first:\n%s", finished)
	}

	// The report for the finished session is read-only and doesn't evaluate,
	// so it survives the manifest going away.
	calls := ev.calls
	ev.eval = nil
	reminded := map[string]int{}
	for _, o := range store.obligations[res.Session.ID] {
		reminded[o.ID] = o.RemindedCount
	}
	report, err := srv.Completeness(ctx, res.Session.ID)
	if err != nil || !report.Enabled || len(report.Obligations) != 2 || ev.calls != calls {
		t.Fatalf("report = %+v, %v (evaluations: %d)", report, err, ev.calls-calls)
	}
	for _, o := range store.obligations[res.Session.ID] {
		if o.Status == ObligationVoid || o.RemindedCount != reminded[o.ID] {
			t.Errorf("a finished session's obligations changed: %+v", o)
		}
	}
}

func TestService_CompletenessReport(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("r", "story", "A", true), finding("r", "test", "A", false))}
	ev.eval.Findings[1].AttestPrompt = "Run `pnpm test`"
	ev.eval.Rules = []RuleInfo{{ID: "r", Describe: "New screen", Requires: []string{"story (required)"}}}
	ev.eval.Warnings = []string{"no merge-base"}
	srv, store, dir := newCompletenessService(t, ev)
	ctx := context.Background()
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	report, err := srv.Completeness(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("Completeness: %v", err)
	}
	if !report.Enabled || report.Summary != (CompletenessSummary{Open: 1, Satisfied: 1}) || len(report.Rules) != 1 || len(report.Stacks) != 1 {
		t.Errorf("report = %+v", report)
	}
	if len(report.Obligations) != 2 || report.Obligations[0].Status != ObligationOpen || report.Obligations[0].Attest != "Run `pnpm test`" {
		t.Errorf("obligations = %+v", report.Obligations)
	}
	if !slices.Equal(report.Warnings, []string{"no merge-base"}) {
		t.Errorf("warnings = %v", report.Warnings)
	}
	for _, o := range store.obligations[res.Session.ID] {
		if o.RequirementID == "test" && o.RemindedCount != 2 {
			t.Errorf("the start contract and tf_completeness should each count as a reminder, got %d", o.RemindedCount)
		}
	}
}

// listHook replaces mockStore's ListObligations, to fake a stale read or a
// failure.
type listHook struct {
	*mockStore
	list func(sessionID string) ([]*Obligation, error)
}

func (l *listHook) ListObligations(ctx context.Context, sessionID string) ([]*Obligation, error) {
	return l.list(sessionID)
}

func TestService_ConcurrentInsertKeepsStoredID(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("r", "story", "Page.tsx", false))}
	store := newMockStore()
	hook := &listHook{mockStore: store, list: func(string) ([]*Obligation, error) { return nil, nil }}
	srv := NewService(hook, &mockGit{ctx: &GitContext{Branch: "main", SHA: "abc123"}}, nil).WithCompleteness(ev)
	ctx := context.Background()

	res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	stored := store.obligations[sid]
	if len(stored) != 1 {
		t.Fatalf("Start should record one obligation, got %d", len(stored))
	}

	// The list read misses the row (another process inserted it after), so
	// the merge mints a new ID; the upsert keeps the stored one.
	cp, err := srv.Checkpoint(ctx, sid, "work", nil, false)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if len(store.obligations[sid]) != 1 {
		t.Fatalf("duplicate obligation rows: %+v", store.obligations[sid])
	}
	c := cp.Completeness
	if c == nil || len(c.NewlyOpen) != 1 || c.NewlyOpen[0].ObligationID != stored[0].ID {
		t.Fatalf("newly_open = %+v, want the stored ID %s", c, stored[0].ID)
	}
	// One reminder from the start contract, one from the checkpoint.
	if got := store.obligation(stored[0].ID).RemindedCount; got != 2 {
		t.Errorf("reminder counted on a phantom ID: stored reminded_count = %d", got)
	}
}

func TestService_ObligationsUnreadable(t *testing.T) {
	ev := &fakeEvaluator{baseSHA: "base", eval: evalWith(finding("r", "story", "Page.tsx", false))}
	store := newMockStore()
	failing := false
	hook := &listHook{mockStore: store, list: func(id string) ([]*Obligation, error) {
		if failing {
			return nil, errors.New("database is locked")
		}
		return store.ListObligations(context.Background(), id)
	}}
	srv := NewService(hook, &mockGit{ctx: &GitContext{Branch: "main", SHA: "abc123"}}, nil).WithCompleteness(ev)
	ctx := context.Background()

	res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	failing = true
	cp, err := srv.Checkpoint(ctx, res.Session.ID, "work", nil, false)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if cp.Completeness == nil || !strings.Contains(cp.Completeness.Error, "database is locked") {
		t.Errorf("checkpoint completeness = %+v, want the read error", cp.Completeness)
	}
	if _, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	comments := store.comments(t)
	for _, c := range comments {
		if strings.Contains(c, "nothing owed") || strings.Contains(c, "Nothing owed") {
			t.Errorf("comment claims nothing is owed after a failed read: %q", c)
		}
	}
	if last := comments[len(comments)-1]; !strings.Contains(last, "### Completeness\nUnavailable: stored obligations couldn't be read") {
		t.Errorf("finished comment = %q", last)
	}
}
