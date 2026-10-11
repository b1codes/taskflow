package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// upsertHook replaces mockStore's UpsertObligation, to fake a failed write
// (e.g. SQLITE_BUSY while the MCP server and the CLI write at once).
type upsertHook struct {
	*mockStore
	upsert func(o *Obligation) error
}

func (u *upsertHook) UpsertObligation(ctx context.Context, o *Obligation) error {
	if err := u.upsert(o); err != nil {
		return err
	}
	return u.mockStore.UpsertObligation(ctx, o)
}

func TestService_PlanSaveFailure(t *testing.T) {
	ctx := context.Background()
	ev := evalWith()
	ev.Rules = []RuleInfo{{ID: "react-screen", Plannable: true, PlanHints: []string{"screen"}, PlanVars: []string{"name"}}}
	store := newMockStore()
	failing := false
	hook := &upsertHook{mockStore: store, upsert: func(o *Obligation) error {
		if failing && o.RequirementID == "story" {
			return errors.New("database is locked")
		}
		return nil
	}}
	srv := NewService(hook, &mockGit{ctx: &GitContext{}}, &mockClickUp{taskName: "Build the checkout screen"}).
		WithCompleteness(&fakeEvaluator{eval: ev, plan: plannedScreen})
	res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	before := store.sessions[sid].PlanState

	failing = true
	item := PlanItem{RuleID: "react-screen", Vars: map[string]string{"name": "Checkout"}}
	plan, err := srv.Plan(ctx, sid, []PlanItem{item}, "")
	if plan != nil || err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "react-screen/story for Checkout.tsx") {
		t.Fatalf("Plan = %+v, %v; want the failed write", plan, err)
	}
	if got := store.sessions[sid].PlanState; got != before {
		t.Errorf("plan state = %q after a failed save, want %q", got, before)
	}

	// Retrying once the store recovers records the plan.
	failing = false
	plan, err = srv.Plan(ctx, sid, []PlanItem{item}, "")
	if err != nil || plan.PlanState != PlanPlanned || store.sessions[sid].PlanState != PlanPlanned || len(store.obligations[sid]) != 2 {
		t.Fatalf("retried Plan = %+v, %v; obligations %d", plan, err, len(store.obligations[sid]))
	}
}

func TestService_PlanWithoutFindings(t *testing.T) {
	ctx := context.Background()
	ev := evalWith()
	ev.Rules = []RuleInfo{{ID: "rn-screen", Stack: "mobile", Plannable: true, PlanVars: []string{"name"}}}
	// The stack's root is missing, so the evaluator finds nothing for the
	// planned subject.
	nothing := func(item PlanItem) (PlannedSubject, []CompletenessFinding, error) {
		return PlannedSubject{RuleID: item.RuleID, Subject: "mobile/" + item.Vars["name"] + ".tsx"}, nil, nil
	}
	srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: ev, plan: nothing})
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	before := store.sessions[sid].PlanState

	plan, err := srv.Plan(ctx, sid, []PlanItem{{RuleID: "rn-screen", Vars: map[string]string{"name": "Checkout"}}}, "")
	if plan != nil || !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Plan = %+v, %v; want ErrInvalidPlan", plan, err)
	}
	if msg := err.Error(); !strings.Contains(msg, `rule "rn-screen" yields no obligations for mobile/Checkout.tsx`) || !strings.Contains(msg, `stack "mobile"`) {
		t.Errorf("error should name the rule, subject, and stack: %v", err)
	}
	if store.sessions[sid].PlanState != before || len(store.obligations[sid]) != 0 {
		t.Errorf("a rejected plan changed the session: state=%q obligations=%d", store.sessions[sid].PlanState, len(store.obligations[sid]))
	}
}

// TestService_BlockedStopWritesNothing: a stop the block gate refuses leaves
// the obligations exactly as they were; one it lets through records the
// stop's evaluation.
func TestService_BlockedStopWritesNothing(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: evalWith(finding("r", "story", "Page.tsx", false), finding("r", "lint", "Old.tsx", false))}
	srv, store, dir := newCompletenessService(t, ev)
	srv.WithGate(GateBlock)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	story, lint := store.find(sid, "story"), store.find(sid, "lint")
	reminded := story.RemindedCount

	// Old.tsx was reverted (its obligation would be voided) and New.tsx
	// added (a new one would be inserted).
	ev.eval = evalWith(finding("r", "story", "Page.tsx", false), finding("r", "e2e", "New.tsx", false))
	_, err = srv.Stop(ctx, sid, "", "COMPLETED", false, false)
	var incomplete *IncompleteSessionError
	if !errors.As(err, &incomplete) {
		t.Fatalf("Stop = %v; want ErrIncompleteSession", err)
	}
	if len(store.obligations[sid]) != 2 || store.find(sid, "e2e") != nil {
		t.Errorf("a blocked stop inserted an obligation: %+v", store.obligations[sid])
	}
	if lint.Status != ObligationOpen {
		t.Errorf("a blocked stop voided an obligation: %+v", lint)
	}
	if story.RemindedCount != reminded {
		t.Errorf("a blocked stop counted a reminder: %d, was %d", story.RemindedCount, reminded)
	}
	// The new obligation's ID isn't in the store yet, and the error says so.
	if len(incomplete.Open) != 2 || len(incomplete.Unrecorded) != 1 || !strings.Contains(err.Error(), "[not recorded yet]") || !strings.Contains(err.Error(), "tf_checkpoint") {
		t.Errorf("open=%+v unrecorded=%v err=%v", incomplete.Open, incomplete.Unrecorded, err)
	}
	if strings.Contains(err.Error(), story.ID+": story for Page.tsx (expected: Page.tsx.story) [not recorded yet]") {
		t.Errorf("a stored obligation is marked unrecorded: %v", err)
	}

	// Under review the stop goes ahead and records its evaluation.
	srv.WithGate(GateReview)
	stop, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	e2e := store.find(sid, "e2e")
	if e2e == nil || e2e.Status != ObligationOpen || lint.Status != ObligationVoid {
		t.Errorf("an allowed stop must persist: e2e=%+v lint=%+v", e2e, lint)
	}
	var ids []string
	for _, v := range stop.Gate.OpenRequired {
		ids = append(ids, v.ObligationID)
	}
	slices.Sort(ids)
	want := []string{story.ID, e2e.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("gate open = %v, want the stored IDs %v", ids, want)
	}
}

// TestService_StopFollowsStoredIDs: when another writer inserted an
// obligation the stop's read missed, the stop result names the stored ID.
func TestService_StopFollowsStoredIDs(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: evalWith(finding("r", "story", "Page.tsx", false))}
	store := newMockStore()
	stale := false
	hook := &listHook{mockStore: store, list: func(id string) ([]*Obligation, error) {
		if stale {
			return nil, nil
		}
		return store.ListObligations(context.Background(), id)
	}}
	srv := NewService(hook, &mockGit{ctx: &GitContext{}}, nil).WithCompleteness(ev)
	res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	story := store.find(sid, "story")

	stale = true
	stop, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(store.obligations[sid]) != 1 {
		t.Fatalf("duplicate obligation rows: %+v", store.obligations[sid])
	}
	if open := stop.Gate.OpenRequired; len(open) != 1 || open[0].ObligationID != story.ID {
		t.Errorf("gate open = %+v, want the stored ID %s", open, story.ID)
	}
}
