package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestChecklistItem_Names(t *testing.T) {
	tests := []struct {
		name     string
		status   ObligationStatus
		levels   []string
		want     string
		resolved bool
	}{
		{"open", ObligationOpen, nil, "[e2e] e2e for A", false},
		{"satisfied by checks", ObligationSatisfied, []string{"structural"}, "[e2e] e2e for A — structural ✓", true},
		{"attested alone", ObligationAttested, []string{"attest"}, "[e2e] e2e for A — attested", true},
		{"attested, checks passing", ObligationSatisfied, []string{"structural", "attest"}, "[e2e] e2e for A — attested, structural ✓", true},
		{"waived", ObligationWaived, nil, "[e2e] e2e for A — waived", true},
		{"void", ObligationVoid, []string{"structural"}, "~~[e2e] e2e for A~~", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := stored("r", "e2e", "A", tt.status)
			o.VerifiedLevels = tt.levels
			it := checklistItem(o)
			if it.Name != tt.want || it.Resolved != tt.resolved || it.Match != "[e2e] e2e for A" || it.Key != "r/e2e/A" {
				t.Errorf("item = %+v, want name %q resolved %v", it, tt.want, tt.resolved)
			}
			if it.Void != (tt.status == ObligationVoid) {
				t.Errorf("void = %v", it.Void)
			}
			if !ChecklistItemMatches(it.Name, it.Match) {
				t.Errorf("%q doesn't match its own stable part %q", it.Name, it.Match)
			}
		})
	}
}

func TestChecklistItemMatches(t *testing.T) {
	const match = "[story] Stories for Page"
	for name, want := range map[string]bool{
		"[story] Stories for Page":                true,
		"[story] Stories for Page — attested":     true,
		"[story] Stories for Page — structural ✓": true,
		"~~[story] Stories for Page~~":            true,
		"[story] Stories for PageHeader":          false,
		"~~[story] Stories for PageHeader~~":      false,
		"[story] Stories for Page (old)":          false,
		"[story] Stories for Page—attested":       false,
		"[e2e] Stories for Page":                  false,
		"Stories for Page":                        false,
		"[story] stories for page":                false,
	} {
		if got := ChecklistItemMatches(name, match); got != want {
			t.Errorf("ChecklistItemMatches(%q) = %v, want %v", name, got, want)
		}
	}
}

// checklists returns the payload of every queued SYNC_CHECKLIST op, in order.
func (m *mockStore) checklists(t *testing.T) []ChecklistPayload {
	t.Helper()
	var out []ChecklistPayload
	for _, op := range m.syncOps {
		if op.Operation != OpSyncChecklist {
			continue
		}
		var p ChecklistPayload
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			t.Fatalf("invalid SYNC_CHECKLIST payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestService_ChecklistSyncedWhenObligationsChange(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: screenEval()}
	srv, store, dir := newCompletenessService(t, ev)

	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	cls := store.checklists(t)
	if len(cls) != 1 {
		t.Fatalf("Start found new obligations: want 1 checklist op, got %d", len(cls))
	}
	if cls[0].ChecklistName != ChecklistName || len(cls[0].Items) != 2 {
		t.Fatalf("checklist = %+v", cls[0])
	}

	// Nothing changed: nothing is queued.
	if _, err := srv.Checkpoint(ctx, sid, "no change", nil, false); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := srv.Completeness(ctx, sid); err != nil {
		t.Fatalf("Completeness: %v", err)
	}
	if n := len(store.checklists(t)); n != 1 {
		t.Errorf("an evaluation that changes nothing queued a checklist: %d ops", n)
	}

	// The story is written: its item is resolved.
	ev.eval = evalWith(finding("react-screen", "story", "Page.tsx", true), finding("react-screen", "unit-test", "Page.tsx", true))
	if _, err := srv.Checkpoint(ctx, sid, "stories", nil, false); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	cls = store.checklists(t)
	if len(cls) != 2 {
		t.Fatalf("a status change must queue the checklist: %d ops", len(cls))
	}
	for _, it := range cls[1].Items {
		if !it.Resolved {
			t.Errorf("item %+v should be resolved", it)
		}
	}

	// The screen is reverted: both obligations are voided and struck.
	ev.eval = evalWith()
	if _, err := srv.Checkpoint(ctx, sid, "reverted", nil, false); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	cls = store.checklists(t)
	if len(cls) != 3 {
		t.Fatalf("voiding must queue the checklist: %d ops", len(cls))
	}
	for _, it := range cls[2].Items {
		if !it.Void || it.Name != "~~"+it.Match+"~~" {
			t.Errorf("voided item = %+v", it)
		}
	}
}

func TestService_ChecklistOnWaiveAndAttest(t *testing.T) {
	ctx := context.Background()
	e2e := finding("react-screen", "e2e", "Page.tsx", false)
	e2e.AttestPrompt = "Name the spec covering Page"
	ev := &fakeEvaluator{eval: evalWith(e2e, finding("react-screen", "story", "Page.tsx", false))}
	srv, store, dir := newCompletenessService(t, ev)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	before := len(store.checklists(t))

	if _, err := srv.Attest(ctx, sid, store.find(sid, "e2e").ID, "e2e/page.spec.ts"); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	cls := store.checklists(t)
	if len(cls) != before+1 {
		t.Fatalf("attesting must queue the checklist once: %d ops after %d", len(cls), before)
	}
	if it := itemFor(cls[len(cls)-1], "react-screen/e2e/Page.tsx"); it == nil || it.Name != "[e2e] e2e for Page.tsx — attested" || !it.Resolved {
		t.Errorf("attested item = %+v", it)
	}

	// New evidence for the same attestation doesn't change the item.
	if _, err := srv.Attest(ctx, sid, store.find(sid, "e2e").ID, "e2e/page.spec.ts: 'renders'"); err != nil {
		t.Fatalf("Attest again: %v", err)
	}
	if n := len(store.checklists(t)); n != len(cls) {
		t.Errorf("replacing the evidence queued a checklist: %d ops", n)
	}

	if _, err := srv.Waive(ctx, sid, store.find(sid, "story").ID, "no stories for flagged pages"); err != nil {
		t.Fatalf("Waive: %v", err)
	}
	cls = store.checklists(t)
	if it := itemFor(cls[len(cls)-1], "react-screen/story/Page.tsx"); it == nil || it.Name != "[story] story for Page.tsx — waived" || !it.Resolved {
		t.Errorf("waived item = %+v", it)
	}
}

func TestService_ChecklistOnPlan(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: evalWith()}
	ev.plan = func(item PlanItem) (PlannedSubject, []CompletenessFinding, error) {
		subject := item.Vars["name"] + ".tsx"
		return PlannedSubject{RuleID: item.RuleID, Subject: subject},
			[]CompletenessFinding{finding(item.RuleID, "story", subject, false)}, nil
	}
	srv, store, dir := newCompletenessService(t, ev)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := len(store.checklists(t)); n != 0 {
		t.Fatalf("nothing is owed yet, so nothing is queued: %d ops", n)
	}
	items := []PlanItem{{RuleID: "react-screen", Vars: map[string]string{"name": "Checkout"}}}
	if _, err := srv.Plan(ctx, res.Session.ID, items, ""); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	cls := store.checklists(t)
	if len(cls) != 1 || itemFor(cls[0], "react-screen/story/Checkout.tsx") == nil {
		t.Fatalf("the plan must push the checklist: %+v", cls)
	}

	// Planning the same work again changes nothing, but tf_plan still
	// pushes the checklist.
	if _, err := srv.Plan(ctx, res.Session.ID, items, ""); err != nil {
		t.Fatalf("Plan again: %v", err)
	}
	if n := len(store.checklists(t)); n != 2 {
		t.Errorf("tf_plan must always push the checklist: %d ops", n)
	}
}

func TestService_BlockedStopQueuesNoChecklist(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: evalWith()}
	srv, store, dir := newCompletenessService(t, ev)
	srv.WithGate(GateBlock)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev.eval = screenEval()
	_, err = srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false)
	if !errors.Is(err, ErrIncompleteSession) {
		t.Fatalf("Stop = %v, want ErrIncompleteSession", err)
	}
	if n := len(store.checklists(t)); n != 0 {
		t.Errorf("a blocked stop writes nothing, so it must not queue the checklist: %d ops", n)
	}

	// Pausing goes ahead and records what it found.
	if _, err := srv.Stop(ctx, res.Session.ID, "", "PAUSED", false, false); err != nil {
		t.Fatalf("Stop(paused): %v", err)
	}
	if n := len(store.checklists(t)); n != 1 {
		t.Errorf("the pause recorded new obligations, so it must queue the checklist: %d ops", n)
	}
}

func itemFor(p ChecklistPayload, key string) *ChecklistItem {
	for i := range p.Items {
		if p.Items[i].Key == key {
			return &p.Items[i]
		}
	}
	return nil
}
