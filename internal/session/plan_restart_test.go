package session

import (
	"context"
	"testing"
)

// TestService_PlanStateOnRestart: a new session after a finished one for the
// same task keeps its plan decision, so the plan isn't suggested or reminded
// again.
func TestService_PlanStateOnRestart(t *testing.T) {
	ctx := context.Background()
	screenRules := []RuleInfo{{ID: "react-screen", Plannable: true, PlanHints: []string{"screen"}, PlanVars: []string{"feature", "name"}}}
	withRules := func(ev *Evaluation) *Evaluation {
		ev.Rules = screenRules
		return ev
	}
	checkout := PlanItem{RuleID: "react-screen", Vars: map[string]string{"feature": "payments", "name": "Checkout"}}

	// finish starts the task (which suggests a plan), lets decide settle the
	// plan, completes the session, and starts the task again.
	finish := func(t *testing.T, decide func(srv *Service, fe *fakeEvaluator, store *mockStore, sid string)) (*StartResult, *Service, *mockStore, string) {
		t.Helper()
		fe := &fakeEvaluator{eval: withRules(evalWith()), plan: plannedScreen}
		srv, store, dir := newCompletenessService(t, fe)
		srv.clickup = &mockClickUp{taskName: "Build the checkout screens"}
		srv.WithGate(GateReport)
		first, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil || first.AgenticContract.Completeness.SuggestedPlan == nil {
			t.Fatalf("Start = %+v, %v; want a suggested plan", first, err)
		}
		prev := first.Session.ID
		decide(srv, fe, store, prev)
		if _, err := srv.Stop(ctx, prev, "", "COMPLETED", false, false); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		fe.eval = withRules(evalWith())
		second, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if second.Session.ID == prev {
			t.Fatal("restarting a completed task must create a new session")
		}
		return second, srv, store, prev
	}
	// assertSettled checks the new session has state want, no suggested
	// plan, and no plan reminder at checkpoint.
	assertSettled := func(t *testing.T, res *StartResult, srv *Service, store *mockStore, want PlanState) {
		t.Helper()
		sid := res.Session.ID
		if sp := res.AgenticContract.Completeness.SuggestedPlan; sp != nil {
			t.Errorf("suggested plan = %+v; the plan was already %s", sp, want)
		}
		if got := store.sessions[sid].PlanState; got != want {
			t.Errorf("plan state = %q, want %q", got, want)
		}
		cp, err := srv.Checkpoint(ctx, sid, "work", nil, false)
		if err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		if cp.Completeness.PlanReminder != "" {
			t.Errorf("plan reminder = %q; want none", cp.Completeness.PlanReminder)
		}
	}

	t.Run("planned carries over with its obligations", func(t *testing.T) {
		res, srv, store, _ := finish(t, func(srv *Service, _ *fakeEvaluator, _ *mockStore, sid string) {
			if _, err := srv.Plan(ctx, sid, []PlanItem{checkout}, ""); err != nil {
				t.Fatalf("Plan: %v", err)
			}
		})
		if n := len(store.obligations[res.Session.ID]); n != 2 {
			t.Errorf("inherited %d planned obligations, want 2", n)
		}
		assertSettled(t, res, srv, store, PlanPlanned)
	})

	t.Run("planned carries over when the planned work was all done", func(t *testing.T) {
		res, srv, store, _ := finish(t, func(srv *Service, fe *fakeEvaluator, _ *mockStore, sid string) {
			if _, err := srv.Plan(ctx, sid, []PlanItem{checkout}, ""); err != nil {
				t.Fatalf("Plan: %v", err)
			}
			fe.eval = withRules(evalWith(
				finding("react-screen", "subject-file", "Checkout.tsx", true),
				finding("react-screen", "story", "Checkout.tsx", true),
			))
		})
		if n := len(store.obligations[res.Session.ID]); n != 0 {
			t.Fatalf("inherited %d obligations, want none (all satisfied)", n)
		}
		assertSettled(t, res, srv, store, PlanPlanned)
	})

	t.Run("declined carries over", func(t *testing.T) {
		res, srv, store, _ := finish(t, func(srv *Service, _ *fakeEvaluator, _ *mockStore, sid string) {
			if _, err := srv.Plan(ctx, sid, nil, "Copy change only; no new screens"); err != nil {
				t.Fatalf("decline: %v", err)
			}
		})
		assertSettled(t, res, srv, store, PlanDeclined)
	})

	t.Run("planned obligations without a recorded plan count as planned", func(t *testing.T) {
		res, srv, store, _ := finish(t, func(srv *Service, _ *fakeEvaluator, store *mockStore, sid string) {
			if _, err := srv.Plan(ctx, sid, []PlanItem{checkout}, ""); err != nil {
				t.Fatalf("Plan: %v", err)
			}
			// The plan state was lost (e.g. a session from before plan
			// states were carried over); the planned obligations remain.
			store.sessions[sid].PlanState = PlanSuggested
		})
		assertSettled(t, res, srv, store, PlanPlanned)
	})

	t.Run("an unanswered suggestion is suggested again", func(t *testing.T) {
		res, _, store, _ := finish(t, func(*Service, *fakeEvaluator, *mockStore, string) {})
		if res.AgenticContract.Completeness.SuggestedPlan == nil || store.sessions[res.Session.ID].PlanState != PlanSuggested {
			t.Errorf("suggested=%+v state=%q; want the plan suggested again",
				res.AgenticContract.Completeness.SuggestedPlan, store.sessions[res.Session.ID].PlanState)
		}
	})
}

func TestInheritedPlanState(t *testing.T) {
	for prev, want := range map[PlanState]PlanState{
		PlanPlanned:   PlanPlanned,
		PlanDeclined:  PlanDeclined,
		PlanSuggested: PlanNone,
		PlanNone:      PlanNone,
		"":            PlanNone,
	} {
		if got := inheritedPlanState(&Session{PlanState: prev}); got != want {
			t.Errorf("inheritedPlanState(%q) = %q, want %q", prev, got, want)
		}
	}
	if got := inheritedPlanState(nil); got != PlanNone {
		t.Errorf("inheritedPlanState(nil) = %q", got)
	}
}
