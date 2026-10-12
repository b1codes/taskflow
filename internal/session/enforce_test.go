package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// openStory and doneTest are a required obligation still open and one met,
// for a screen.
func screenEval() *Evaluation {
	return evalWith(finding("react-screen", "story", "Page.tsx", false), finding("react-screen", "unit-test", "Page.tsx", true))
}

// ops returns the queued sync operations, in order.
func (m *mockStore) ops() []string {
	var out []string
	for _, op := range m.syncOps {
		out = append(out, op.Operation)
	}
	return out
}

func (m *mockStore) find(sessionID, requirement string) *Obligation {
	for _, o := range m.obligations[sessionID] {
		if o.RequirementID == requirement {
			return o
		}
	}
	return nil
}

func TestMergeObligations_Attestations(t *testing.T) {
	now := time.Now()

	t.Run("passing checks complete an attestation", func(t *testing.T) {
		o := stored("r", "e2e", "A", ObligationAttested)
		o.VerifiedLevels = []string{"attest"}
		f := finding("r", "e2e", "A", false)
		f.ChecksPassed, f.Levels, f.AttestPrompt = true, []string{"structural"}, "Name the spec"
		mr := mergeObligations("s", []*Obligation{o}, evalWith(f), now)
		if o.Status != ObligationSatisfied || o.SatisfiedAt == nil || !slices.Equal(o.VerifiedLevels, []string{"structural", "attest"}) || len(mr.upserts) != 1 {
			t.Errorf("attested + checks = %+v", o)
		}
		if len(mr.newlySatisfied) != 0 {
			t.Errorf("an attested obligation was already resolved, so it isn't newly satisfied")
		}
	})

	t.Run("failing checks fall back to the attestation, never to open", func(t *testing.T) {
		o := stored("r", "e2e", "A", ObligationSatisfied)
		o.VerifiedLevels = []string{"structural", "attest"}
		f := finding("r", "e2e", "A", false)
		f.AttestPrompt = "Name the spec"
		mr := mergeObligations("s", []*Obligation{o}, evalWith(f), now)
		if o.Status != ObligationAttested || o.SatisfiedAt != nil || !slices.Equal(o.VerifiedLevels, []string{"attest"}) || mr.newlyOpen[o.ID] {
			t.Errorf("attested, checks broke = %+v", o)
		}
	})

	t.Run("an attestation isn't voided with its trigger", func(t *testing.T) {
		o := stored("r", "e2e", "Gone", ObligationSatisfied)
		o.VerifiedLevels = []string{"attest"}
		mr := mergeObligations("s", []*Obligation{o}, evalWith(), now)
		if o.Status != ObligationSatisfied || len(mr.voided) != 0 {
			t.Errorf("attested obligation voided: %+v", o)
		}
	})

	t.Run("new findings for a planned subject are planned", func(t *testing.T) {
		ev := evalWith(finding("r", "story", "Planned.tsx", false), finding("r", "story", "Diff.tsx", false))
		ev.Planned = []PlannedSubject{{RuleID: "r", Subject: "Planned.tsx"}}
		mr := mergeObligations("s", nil, ev, now)
		for _, o := range mr.upserts {
			if o.Planned != (o.Subject == "Planned.tsx") {
				t.Errorf("%s planned = %v", o.Subject, o.Planned)
			}
		}
	})
}

func TestService_Gate(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, ev *fakeEvaluator, gate GateMode) (*Service, *mockStore, string) {
		t.Helper()
		srv, store, dir := newCompletenessService(t, ev)
		srv.WithGate(gate)
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return srv, store, res.Session.ID
	}
	finished := func(t *testing.T, store *mockStore) string {
		t.Helper()
		comments := store.comments(t)
		return comments[len(comments)-1]
	}

	t.Run("review is the default: complete locally, task to review", func(t *testing.T) {
		srv, store, sid := start(t, &fakeEvaluator{eval: screenEval()}, "")
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if res.Status != StatusCompleted || store.sessions[sid].Status != StatusCompleted {
			t.Errorf("status = %s / %s", res.Status, store.sessions[sid].Status)
		}
		g := res.Gate
		if g == nil || g.Mode != GateReview || g.Outcome != GateMovedToReview || len(g.OpenRequired) != 1 || g.OpenRequired[0].Requirement != "story" {
			t.Fatalf("gate = %+v", g)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleReview}) {
			t.Errorf("roles = %v, want review in place of complete", roles)
		}
		if c := finished(t, store); !strings.Contains(c, "**Completion gate (review):** 1 required obligation(s) open, so the task goes to review instead of complete") {
			t.Errorf("finished comment = %q", c)
		}
		if res.Completeness == nil || res.Completeness.Summary.Open != 1 {
			t.Errorf("stop result report = %+v", res.Completeness)
		}
	})

	t.Run("report completes", func(t *testing.T) {
		srv, store, sid := start(t, &fakeEvaluator{eval: screenEval()}, GateReport)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Outcome != GateReported {
			t.Fatalf("Stop = %+v, %v", res, err)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleComplete}) {
			t.Errorf("roles = %v", roles)
		}
		if c := finished(t, store); !strings.Contains(c, "**Completion gate (report):** completed with 1 required obligation(s) open") {
			t.Errorf("finished comment = %q", c)
		}
	})

	t.Run("block refuses and changes nothing", func(t *testing.T) {
		srv, store, sid := start(t, &fakeEvaluator{eval: screenEval()}, GateBlock)
		opsBefore := len(store.syncOps)
		story := store.find(sid, "story")
		res, err := srv.Stop(ctx, sid, "all done", "COMPLETED", false, false)
		var incomplete *IncompleteSessionError
		if res != nil || !errors.Is(err, ErrIncompleteSession) || !errors.As(err, &incomplete) {
			t.Fatalf("Stop = %+v, %v; want ErrIncompleteSession", res, err)
		}
		if len(incomplete.Open) != 1 || !strings.Contains(err.Error(), story.ID) || !strings.Contains(err.Error(), "tf_waive") {
			t.Errorf("error should name the open obligation and how to resolve it: %v", err)
		}
		if store.sessions[sid].Status != StatusActive || len(store.syncOps) != opsBefore || len(store.checkpoints[sid]) != 0 {
			t.Errorf("a blocked stop changed the session: status=%s ops=%v checkpoints=%d", store.sessions[sid].Status, store.ops(), len(store.checkpoints[sid]))
		}
	})

	t.Run("block lets a human force completion, recorded as a decision", func(t *testing.T) {
		srv, store, sid := start(t, &fakeEvaluator{eval: screenEval()}, GateBlock)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, true)
		if err != nil || res.Gate.Outcome != GateForced || res.Gate.DecisionID == "" {
			t.Fatalf("forced Stop = %+v, %v", res, err)
		}
		ds := store.decisionsFor(sid)
		if len(ds) != 1 || ds[0].ID != res.Gate.DecisionID || ds[0].Decision != "Completion forced by human with 1 open obligations" ||
			!strings.Contains(ds[0].Rationale, "story for Page.tsx") || !slices.Equal(ds[0].Files, []string{"Page.tsx"}) {
			t.Errorf("decision = %+v", ds)
		}
		if !slices.Contains(store.ops(), "POST_DECISION") {
			t.Errorf("the forced completion must reach ClickUp: %v", store.ops())
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleComplete}) {
			t.Errorf("roles = %v", roles)
		}
	})

	t.Run("force without anything blocked records nothing", func(t *testing.T) {
		srv, store, sid := start(t, &fakeEvaluator{eval: evalWith(finding("r", "q", "A", true))}, GateBlock)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, true)
		if err != nil || res.Gate.Outcome != GatePassed || len(store.decisionsFor(sid)) != 0 {
			t.Errorf("Stop = %+v, %v, decisions %d", res.Gate, err, len(store.decisionsFor(sid)))
		}
	})

	t.Run("recommended work never blocks", func(t *testing.T) {
		f := finding("r", "visual", "A", false)
		f.Severity = "recommended"
		srv, _, sid := start(t, &fakeEvaluator{eval: evalWith(f)}, GateBlock)
		if res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); err != nil || res.Gate.Outcome != GatePassed {
			t.Errorf("Stop = %+v, %v", res, err)
		}
	})

	t.Run("pausing and archiving are never gated", func(t *testing.T) {
		for _, target := range []string{"PAUSED", "ARCHIVED"} {
			srv, _, sid := start(t, &fakeEvaluator{eval: screenEval()}, GateBlock)
			res, err := srv.Stop(ctx, sid, "", target, false, false)
			if err != nil || res.Gate != nil {
				t.Errorf("%s: Stop = %+v, %v", target, res, err)
			}
		}
	})

	t.Run("the manifest can raise the configured gate", func(t *testing.T) {
		ev := screenEval()
		ev.Gate = "block"
		srv, _, sid := start(t, &fakeEvaluator{eval: ev}, GateReport)
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrIncompleteSession) {
			t.Errorf("Stop = %v; want the manifest's block", err)
		}
	})

	t.Run("the manifest can't lower the configured gate", func(t *testing.T) {
		for _, lower := range []string{"report", "review"} {
			ev := screenEval()
			ev.Gate = lower
			srv, _, sid := start(t, &fakeEvaluator{eval: ev}, GateBlock)
			if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrIncompleteSession) {
				t.Errorf("manifest gate %s: Stop = %v; want config's block", lower, err)
			}
		}
		ev := screenEval()
		ev.Gate = "report"
		srv, _, sid := start(t, &fakeEvaluator{eval: ev}, "")
		if res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); err != nil || res.Gate.Mode != GateReview {
			t.Errorf("Stop = %+v, %v; want the default review", res, err)
		}
	})

	t.Run("an invalid manifest can't lower the configured gate", func(t *testing.T) {
		invalid := &Evaluation{Manifest: ".agents/completeness.yaml", Error: "version: must be 1"}

		ev := &fakeEvaluator{eval: screenEval()}
		srv, store, sid := start(t, ev, GateBlock)
		ev.eval = invalid
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrCompletenessUnchecked) || !strings.Contains(err.Error(), "version: must be 1") {
			t.Errorf("block: Stop = %v; want ErrCompletenessUnchecked", err)
		}
		if store.sessions[sid].Status != StatusActive {
			t.Errorf("block: status = %s", store.sessions[sid].Status)
		}

		ev = &fakeEvaluator{eval: screenEval()}
		srv, store, sid = start(t, ev, GateReview)
		ev.eval = invalid
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Outcome != GateMovedToReview || !strings.Contains(res.Gate.Unchecked, "is invalid") {
			t.Fatalf("review: Stop = %+v, %v", res.Gate, err)
		}
		// The last known open work is still listed.
		if len(res.Gate.OpenRequired) != 1 || res.Gate.OpenRequired[0].Requirement != "story" {
			t.Errorf("review: open = %+v", res.Gate.OpenRequired)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleReview}) {
			t.Errorf("review: roles = %v", roles)
		}
	})
}

func TestService_Waive(t *testing.T) {
	ctx := context.Background()
	srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: screenEval()})
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	story, test := store.find(sid, "story"), store.find(sid, "unit-test")

	if _, err := srv.Waive(ctx, sid, story.ID, "  "); err == nil {
		t.Error("a waiver needs a rationale")
	}
	_, err = srv.Waive(ctx, sid, "nope", "n/a")
	if !errors.Is(err, ErrUnknownObligation) || !strings.Contains(err.Error(), story.ID) || !strings.Contains(err.Error(), "story for Page.tsx") {
		t.Errorf("unknown obligation error should list the open ones: %v", err)
	}
	if _, err := srv.Waive(ctx, sid, test.ID, "n/a"); !errors.Is(err, ErrUnknownObligation) || !strings.Contains(err.Error(), "already satisfied") {
		t.Errorf("waiving a satisfied obligation = %v", err)
	}

	got, err := srv.Waive(ctx, sid, story.ID, "Page is behind a flag with no stories yet")
	if err != nil {
		t.Fatalf("Waive: %v", err)
	}
	ds := store.decisionsFor(sid)
	if len(ds) != 1 || ds[0].Topic != "Completeness waiver: story for Page.tsx" || ds[0].Status != "rejected_alternative" ||
		ds[0].Rationale != "Page is behind a flag with no stories yet" || !slices.Equal(ds[0].Files, []string{"Page.tsx"}) {
		t.Fatalf("decision = %+v", ds)
	}
	if got.DecisionID != ds[0].ID || got.Obligation.Status != ObligationWaived || got.Obligation.Evidence != ds[0].Rationale {
		t.Errorf("result = %+v", got)
	}
	if got.Summary != (CompletenessSummary{Satisfied: 1, Waived: 1}) || len(got.StillOpen) != 0 {
		t.Errorf("summary = %+v still_open = %+v", got.Summary, got.StillOpen)
	}
	if o := store.obligation(story.ID); o.Status != ObligationWaived || o.WaiverDecisionID != ds[0].ID {
		t.Errorf("stored = %+v", o)
	}
	if ops := store.ops(); !slices.Equal(ops[len(ops)-2:], []string{"POST_DECISION", OpSyncChecklist}) {
		t.Errorf("the waiver must be posted to ClickUp as a decision, then shown in the checklist: %v", ops)
	}
	if _, err := srv.Waive(ctx, sid, story.ID, "again"); !errors.Is(err, ErrUnknownObligation) {
		t.Errorf("waiving twice = %v", err)
	}

	// The waiver is sticky across evaluations, and the stop report shows
	// its rationale.
	if _, err := srv.Checkpoint(ctx, sid, "more", nil, false); err != nil {
		t.Fatal(err)
	}
	if o := store.obligation(story.ID); o.Status != ObligationWaived {
		t.Errorf("waiver didn't stick: %s", o.Status)
	}
	stop, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
	if err != nil || stop.Gate.Outcome != GatePassed {
		t.Fatalf("Stop = %+v, %v; a waived obligation no longer holds up completion", stop, err)
	}
	comments := store.comments(t)
	if c := comments[len(comments)-1]; !strings.Contains(c, "| story for Page.tsx | `Page.tsx` | required | waived | — | Page is behind a flag with no stories yet |") {
		t.Errorf("finished comment = %q", c)
	}
	if _, err := srv.Waive(ctx, sid, story.ID, "late"); err == nil || !strings.Contains(err.Error(), "non-active") {
		t.Errorf("waiving on a finished session = %v", err)
	}
}

func TestService_WaiveVoid(t *testing.T) {
	ctx := context.Background()
	ev := &fakeEvaluator{eval: screenEval()}
	srv, store, dir := newCompletenessService(t, ev)
	res, _ := srv.Start(ctx, "task-1", dir, false)
	story := store.find(res.Session.ID, "story")
	ev.eval = evalWith() // the page was reverted
	if _, err := srv.Checkpoint(ctx, res.Session.ID, "revert", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Waive(ctx, res.Session.ID, story.ID, "n/a"); !errors.Is(err, ErrUnknownObligation) || !strings.Contains(err.Error(), "no longer applies") {
		t.Errorf("waiving a void obligation = %v", err)
	}
}

func TestService_Attest(t *testing.T) {
	ctx := context.Background()
	e2e := finding("react-screen", "e2e", "Page.tsx", false)
	e2e.AttestPrompt = "Name the spec covering Page"
	contract := finding("react-screen", "contract", "Page.tsx", false)
	contract.AttestPrompt = "Name the contract test covering Page"
	ev := &fakeEvaluator{eval: evalWith(e2e, finding("react-screen", "story", "Page.tsx", false), contract)}
	srv, store, dir := newCompletenessService(t, ev)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	id := store.find(sid, "e2e").ID

	if _, err := srv.Attest(ctx, sid, id, ""); err == nil {
		t.Error("an attestation needs evidence")
	}
	if _, err := srv.Attest(ctx, sid, "nope", "x"); !errors.Is(err, ErrUnknownObligation) || !strings.Contains(err.Error(), id) {
		t.Errorf("unknown obligation = %v", err)
	}

	// The spec isn't in the diff yet: the claim stands alone.
	calls := ev.calls
	got, err := srv.Attest(ctx, sid, id, "e2e/checkout.spec.ts: 'pays with a card'")
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if n := ev.calls - calls; n != 1 {
		t.Errorf("Attest evaluated %d times, want once", n)
	}
	if got.Obligation.Status != ObligationAttested || got.Obligation.Evidence != "e2e/checkout.spec.ts: 'pays with a card'" ||
		got.Obligation.Attest != "Name the spec covering Page" || got.Summary != (CompletenessSummary{Open: 2, Attested: 1}) || len(got.StillOpen) != 2 {
		t.Errorf("attested = %+v", got)
	}
	if o := store.obligation(id); o.Status != ObligationAttested || o.SatisfiedAt != nil || !slices.Equal(o.VerifiedLevels, []string{"attest"}) {
		t.Errorf("stored = %+v", o)
	}
	if slices.Contains(store.ops(), "POST_DECISION") {
		t.Error("attestations don't post comments")
	}

	// The spec lands: with the attestation, the requirement is met.
	ev.eval.Findings[0].ChecksPassed, ev.eval.Findings[0].Levels = true, []string{"structural"}
	cp, err := srv.Checkpoint(ctx, sid, "spec", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if o := store.obligation(id); o.Status != ObligationSatisfied || !slices.Equal(o.VerifiedLevels, []string{"structural", "attest"}) {
		t.Errorf("attested + checks = %+v", o)
	}
	if cp.Completeness.Summary != (CompletenessSummary{Open: 2, Satisfied: 1}) {
		t.Errorf("summary = %+v", cp.Completeness.Summary)
	}

	// Replacing the evidence while the checks pass keeps it satisfied.
	got, err = srv.Attest(ctx, sid, id, "e2e/checkout.spec.ts: 'pays with a card', 'retries'")
	if err != nil || got.Obligation.Status != ObligationSatisfied {
		t.Fatalf("re-attesting = %+v, %v", got, err)
	}
	if o := store.obligation(id); o.Status != ObligationSatisfied || o.SatisfiedAt == nil || o.Evidence != "e2e/checkout.spec.ts: 'pays with a card', 'retries'" ||
		!slices.Equal(o.VerifiedLevels, []string{"structural", "attest"}) {
		t.Errorf("stored = %+v", o)
	}

	// Attesting a waived obligation that asks for an attestation takes the
	// waiver back, and unlinks the waiver's decision.
	c := store.find(sid, "contract")
	if _, err := srv.Waive(ctx, sid, c.ID, "not needed"); err != nil {
		t.Fatal(err)
	}
	if o := store.obligation(c.ID); o.WaiverDecisionID == "" {
		t.Fatalf("waiver not linked: %+v", o)
	}
	ev.eval.Findings[2].ChecksPassed = true
	got, err = srv.Attest(ctx, sid, c.ID, "contract/page_test.go covers it after all")
	if err != nil || got.Obligation.Status != ObligationSatisfied || got.Summary != (CompletenessSummary{Open: 1, Satisfied: 2}) {
		t.Fatalf("attesting a waived obligation = %+v, %v", got, err)
	}
	if o := store.obligation(c.ID); o.Status != ObligationSatisfied || o.WaiverDecisionID != "" || o.SatisfiedAt == nil || o.Evidence != "contract/page_test.go covers it after all" {
		t.Errorf("stored after taking back the waiver = %+v", o)
	}

	// An obligation its checks already satisfy has nothing to attest.
	plain := &fakeEvaluator{eval: evalWith(finding("r", "q", "A", true))}
	srv2, store2, dir2 := newCompletenessService(t, plain)
	res2, _ := srv2.Start(ctx, "task-2", dir2, false)
	if _, err := srv2.Attest(ctx, res2.Session.ID, store2.find(res2.Session.ID, "q").ID, "x"); !errors.Is(err, ErrUnknownObligation) || !strings.Contains(err.Error(), "already satisfied") {
		t.Errorf("attesting a satisfied obligation = %v", err)
	}
}

// TestService_AttestRefusesFileChecks: an attestation can't stand in for
// work checked from files, so it can't get past the completion gate.
func TestService_AttestRefusesFileChecks(t *testing.T) {
	ctx := context.Background()
	srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: screenEval()})
	srv.WithGate(GateBlock)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	story := store.find(sid, "story")

	_, err = srv.Attest(ctx, sid, story.ID, "done")
	if !errors.Is(err, ErrNotAttestable) || !strings.Contains(err.Error(), "asks for no attestation") ||
		!strings.Contains(err.Error(), "Page.tsx.story") || !strings.Contains(err.Error(), "tf_waive") {
		t.Errorf("attesting a file-checked obligation = %v", err)
	}
	if o := store.obligation(story.ID); o.Status != ObligationOpen || o.Evidence != "" || slices.Contains(o.VerifiedLevels, levelAttest) {
		t.Errorf("a refused attestation changed the obligation: %+v", o)
	}
	if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrIncompleteSession) {
		t.Errorf("Stop = %v; the block gate must still refuse", err)
	}

	// A waiver of it can't be taken back by attesting either.
	if _, err := srv.Waive(ctx, sid, story.ID, "behind a flag"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Attest(ctx, sid, story.ID, "done"); !errors.Is(err, ErrNotAttestable) {
		t.Errorf("attesting a waived file-checked obligation = %v", err)
	}
	if o := store.obligation(story.ID); o.Status != ObligationWaived || o.WaiverDecisionID == "" || o.Evidence != "behind a flag" {
		t.Errorf("a refused attestation changed the waiver: %+v", o)
	}
}

// TestService_AttestUnchecked: an attestation that can't be checked against
// its requirement is refused, never accepted blind.
func TestService_AttestUnchecked(t *testing.T) {
	ctx := context.Background()
	e2e := finding("react-screen", "e2e", "Page.tsx", false)
	e2e.AttestPrompt = "Name the spec"
	start := func(t *testing.T) (*Service, *mockStore, *fakeEvaluator, string, string) {
		t.Helper()
		ev := &fakeEvaluator{eval: evalWith(e2e)}
		srv, store, dir := newCompletenessService(t, ev)
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return srv, store, ev, res.Session.ID, store.find(res.Session.ID, "e2e").ID
	}
	refused := func(t *testing.T, store *mockStore, err error, id, want string) {
		t.Helper()
		if !errors.Is(err, ErrCompletenessUnchecked) || !strings.Contains(err.Error(), want) {
			t.Errorf("Attest = %v; want ErrCompletenessUnchecked (%s)", err, want)
		}
		if o := store.obligation(id); o.Status != ObligationOpen || o.Evidence != "" {
			t.Errorf("a refused attestation changed the obligation: %+v", o)
		}
	}

	t.Run("invalid manifest", func(t *testing.T) {
		srv, store, ev, sid, id := start(t)
		ev.eval = &Evaluation{Manifest: ".agents/completeness.yaml", Error: "version: must be 1"}
		_, err := srv.Attest(ctx, sid, id, "e2e/page.spec.ts")
		refused(t, store, err, id, "version: must be 1")
	})

	t.Run("no manifest", func(t *testing.T) {
		srv, store, ev, sid, id := start(t)
		ev.eval = nil
		_, err := srv.Attest(ctx, sid, id, "e2e/page.spec.ts")
		refused(t, store, err, id, "no completeness manifest")
	})

	t.Run("failed evaluation", func(t *testing.T) {
		srv, store, ev, sid, id := start(t)
		ev.err = errors.New("permission denied")
		_, err := srv.Attest(ctx, sid, id, "e2e/page.spec.ts")
		refused(t, store, err, id, "permission denied")
	})

	t.Run("completeness off", func(t *testing.T) {
		srv, store, _, sid, id := start(t)
		srv.WithCompleteness(nil)
		_, err := srv.Attest(ctx, sid, id, "e2e/page.spec.ts")
		refused(t, store, err, id, "not enabled")
	})

	t.Run("unreadable obligations", func(t *testing.T) {
		store := newMockStore()
		failing := false
		hook := &listHook{mockStore: store, list: func(id string) ([]*Obligation, error) {
			if failing {
				return nil, errors.New("database is locked")
			}
			return store.ListObligations(context.Background(), id)
		}}
		srv := NewService(hook, &mockGit{ctx: &GitContext{}}, nil).WithCompleteness(&fakeEvaluator{eval: evalWith(e2e)})
		res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		id := store.find(res.Session.ID, "e2e").ID
		failing = true
		_, err = srv.Attest(ctx, res.Session.ID, id, "e2e/page.spec.ts")
		refused(t, store, err, id, "database is locked")
	})
}

// plannedScreen resolves plan items for react-screen to Name.tsx, with a
// subject-file and a story finding.
func plannedScreen(item PlanItem) (PlannedSubject, []CompletenessFinding, error) {
	if item.RuleID != "react-screen" {
		return PlannedSubject{}, nil, errors.New("unknown rule " + item.RuleID)
	}
	subject := item.Vars["name"] + ".tsx"
	return PlannedSubject{RuleID: item.RuleID, Subject: subject}, []CompletenessFinding{
		finding("react-screen", "subject-file", subject, false),
		finding("react-screen", "story", subject, false),
	}, nil
}

func TestService_Plan(t *testing.T) {
	ctx := context.Background()
	ev := evalWith()
	ev.Rules = []RuleInfo{{ID: "react-screen", Describe: "New screen", Plannable: true, PlanHints: []string{"screen"}, PlanVars: []string{"feature", "name"}}}
	fe := &fakeEvaluator{eval: ev, plan: plannedScreen}
	srv, store, dir := newCompletenessService(t, fe)
	srv.clickup = &mockClickUp{taskName: "Build the checkout screens"}

	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := res.Session.ID
	sp := res.AgenticContract.Completeness.SuggestedPlan
	if sp == nil || sp.Reason != `Task name mentions "screen"` || !slices.Equal(sp.Rules, []string{"react-screen"}) ||
		sp.Example.RuleID != "react-screen" || sp.Example.Vars["name"] != "<name>" || sp.Example.Vars["feature"] != "<feature>" {
		t.Fatalf("suggested plan = %+v", sp)
	}
	if store.sessions[sid].PlanState != PlanSuggested {
		t.Errorf("plan state = %q, want suggested", store.sessions[sid].PlanState)
	}

	// Every checkpoint reminds until the agent plans or declines.
	for range 2 {
		cp, _ := srv.Checkpoint(ctx, sid, "work", nil, false)
		if !strings.Contains(cp.Completeness.PlanReminder, "react-screen") {
			t.Errorf("plan reminder = %q", cp.Completeness.PlanReminder)
		}
	}

	if _, err := srv.Plan(ctx, sid, nil, ""); err == nil {
		t.Error("a plan needs items or a decline reason")
	}
	if _, err := srv.Plan(ctx, sid, []PlanItem{{RuleID: "react-screen"}}, "no"); err == nil {
		t.Error("items and a decline reason are exclusive")
	}
	if _, err := srv.Plan(ctx, sid, []PlanItem{{RuleID: "api-route"}}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("an invalid item = %v", err)
	}
	if store.sessions[sid].PlanState != PlanSuggested || len(store.obligations[sid]) != 0 {
		t.Error("a rejected plan must change nothing")
	}

	plan, err := srv.Plan(ctx, sid, []PlanItem{{RuleID: "react-screen", Vars: map[string]string{"feature": "payments", "name": "Checkout"}}}, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.PlanState != PlanPlanned || store.sessions[sid].PlanState != PlanPlanned || len(plan.Planned) != 2 || plan.Summary.Open != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	for _, o := range store.obligations[sid] {
		if !o.Planned || o.Subject != "Checkout.tsx" || o.RemindedCount != 1 {
			t.Errorf("planned obligation = %+v", o)
		}
	}
	if slices.Contains(store.ops(), "POST_DECISION") {
		t.Error("a plan isn't a decision")
	}

	// Planned obligations aren't voided while their file doesn't exist, and
	// the reminder stops.
	cp, _ := srv.Checkpoint(ctx, sid, "work", nil, false)
	if cp.Completeness.PlanReminder != "" || cp.Completeness.Summary.Open != 2 {
		t.Errorf("after planning: %+v", cp.Completeness)
	}
	if _, err := srv.Plan(ctx, sid, nil, "changed my mind"); err == nil {
		t.Error("declining after planning should fail")
	}
}

func TestService_PlanDecline(t *testing.T) {
	ctx := context.Background()
	ev := evalWith()
	ev.Rules = []RuleInfo{{ID: "react-screen", Plannable: true, PlanHints: []string{"screen"}}}
	srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: ev})
	srv.clickup = &mockClickUp{taskName: "Fix the login screen's copy"}
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil || res.AgenticContract.Completeness.SuggestedPlan == nil {
		t.Fatalf("Start = %+v, %v", res, err)
	}
	sid := res.Session.ID

	plan, err := srv.Plan(ctx, sid, nil, "Copy change only; no new screens")
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	ds := store.decisionsFor(sid)
	if plan.PlanState != PlanDeclined || store.sessions[sid].PlanState != PlanDeclined || len(ds) != 1 ||
		ds[0].ID != plan.DecisionID || ds[0].Rationale != "Copy change only; no new screens" {
		t.Errorf("decline = %+v, decisions %+v", plan, ds)
	}
	cp, _ := srv.Checkpoint(ctx, sid, "copy", nil, false)
	if cp.Completeness.PlanReminder != "" {
		t.Error("a declined plan isn't reminded")
	}

	// Resuming doesn't suggest the plan again.
	if _, err := srv.Stop(ctx, sid, "", "PAUSED", false, false); err != nil {
		t.Fatal(err)
	}
	res, _ = srv.Start(ctx, "task-1", dir, false)
	if res.AgenticContract.Completeness.SuggestedPlan != nil || store.sessions[sid].PlanState != PlanDeclined {
		t.Errorf("resumed: suggested=%+v state=%q", res.AgenticContract.Completeness.SuggestedPlan, store.sessions[sid].PlanState)
	}
}

func TestSuggestPlan(t *testing.T) {
	rules := []RuleInfo{
		{ID: "react-screen", Plannable: true, PlanHints: []string{"screen", "page", "view"}, PlanVars: []string{"name"}},
		{ID: "api-route", Plannable: true, PlanHints: []string{"new endpoint"}},
		{ID: "hinted-but-unplannable", PlanHints: []string{"screen"}},
	}
	tests := []struct {
		name, taskName, desc string
		wantRules            []string
		wantReason           string
	}{
		{"name, case-insensitive", "Add Checkout SCREEN", "", []string{"react-screen"}, `Task name mentions "screen"`},
		{"plural", "Settings pages", "", []string{"react-screen"}, `Task name mentions "page"`},
		{"description", "Payments", "Add a new endpoint and its page", []string{"react-screen", "api-route"}, `Task description mentions "page"`},
		{"phrase", "Expose a new endpoints list", "", []string{"api-route"}, `Task name mentions "new endpoint"`},
		{"not a substring", "Address review feedback on the screenshot tool", "", nil, ""},
		{"no match", "Fix flaky test", "Retry the sync worker", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := suggestPlan(rules, tt.taskName, tt.desc)
			if tt.wantRules == nil {
				if sp != nil {
					t.Errorf("suggested %+v, want none", sp)
				}
				return
			}
			if sp == nil || !slices.Equal(sp.Rules, tt.wantRules) || sp.Reason != tt.wantReason {
				t.Errorf("suggested %+v, want rules %v (%s)", sp, tt.wantRules, tt.wantReason)
			}
		})
	}
}

func TestService_InheritOnRestart(t *testing.T) {
	ctx := context.Background()
	e2e := finding("react-screen", "e2e", "Page.tsx", false)
	e2e.AttestPrompt = "Name the spec"
	ev := &fakeEvaluator{eval: evalWith(
		finding("react-screen", "story", "Page.tsx", false),
		finding("react-screen", "unit-test", "Page.tsx", true),
		e2e,
		finding("react-screen", "contract", "Page.tsx", false),
	)}
	srv, store, dir := newCompletenessService(t, ev)
	srv.WithGate(GateReport)
	first, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	prev := first.Session.ID
	waived, attested := store.find(prev, "contract"), store.find(prev, "e2e")
	if _, err := srv.Waive(ctx, prev, waived.ID, "No services changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Attest(ctx, prev, attested.ID, "e2e/page.spec.ts"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Stop(ctx, prev, "", "COMPLETED", false, false); err != nil {
		t.Fatal(err)
	}

	// Rework: the new session's diff no longer touches the page.
	// Page.tsx still exists, so the inherited obligations' rechecks find it.
	ev.eval = evalWith(finding("react-screen", "story", "Page.tsx", false), e2e, finding("react-screen", "contract", "Page.tsx", false))
	second, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	sid := second.Session.ID
	if sid == prev {
		t.Fatal("restarting a completed task must create a new session")
	}
	byReq := map[string]*Obligation{}
	for _, o := range store.obligations[sid] {
		byReq[o.RequirementID] = o
	}
	if len(byReq) != 3 || byReq["unit-test"] != nil {
		t.Fatalf("inherited = %v; want open, attested, and waived, not satisfied", byReq)
	}
	if o := byReq["contract"]; o.Status != ObligationWaived || o.WaiverDecisionID == "" || o.Evidence != "No services changed" || o.InheritedFrom != waived.ID {
		t.Errorf("inherited waiver = %+v", o)
	}
	if o := byReq["e2e"]; o.Status != ObligationAttested || o.Evidence != "e2e/page.spec.ts" || o.InheritedFrom != attested.ID {
		t.Errorf("inherited attestation = %+v", o)
	}
	if o := byReq["story"]; o.Status != ObligationOpen {
		t.Errorf("inherited open obligation = %+v; it isn't voided though its file isn't in the new diff", o)
	}

	cc := second.AgenticContract.Completeness
	if len(cc.Inherited) != 3 || len(cc.Open) != 1 || cc.Open[0].Requirement != "story" {
		t.Fatalf("contract inherited=%+v open=%+v", cc.Inherited, cc.Open)
	}
	for _, v := range cc.Inherited {
		if v.FromSession != prev || v.Status == "" {
			t.Errorf("inherited view = %+v", v)
		}
	}
	if cc.Summary != (CompletenessSummary{Open: 1, Attested: 1, Waived: 1}) {
		t.Errorf("summary = %+v", cc.Summary)
	}

	// The previous session's record is untouched.
	if o := store.obligation(waived.ID); o.SessionID != prev || o.Status != ObligationWaived {
		t.Errorf("source obligation changed: %+v", o)
	}
}

// TestService_GateFailsClosed: under block, a stop that can't check
// completeness must not complete, since nothing shows the work is done.
func TestService_GateFailsClosed(t *testing.T) {
	ctx := context.Background()

	// unreadable starts a session whose obligations can't be read at stop.
	unreadable := func(t *testing.T, ev *Evaluation, gate GateMode) (*Service, *mockStore, string) {
		t.Helper()
		store := newMockStore()
		failing := false
		hook := &listHook{mockStore: store, list: func(id string) ([]*Obligation, error) {
			if failing {
				return nil, errors.New("database is locked")
			}
			return store.ListObligations(context.Background(), id)
		}}
		srv := NewService(hook, &mockGit{ctx: &GitContext{}}, nil).WithCompleteness(&fakeEvaluator{eval: ev}).WithGate(gate)
		res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		failing = true
		return srv, store, res.Session.ID
	}

	t.Run("unreadable obligations block", func(t *testing.T) {
		srv, store, sid := unreadable(t, screenEval(), GateBlock)
		opsBefore := len(store.syncOps)
		res, err := srv.Stop(ctx, sid, "done", "COMPLETED", false, false)
		if res != nil || !errors.Is(err, ErrCompletenessUnchecked) || !strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("Stop = %+v, %v; want ErrCompletenessUnchecked", res, err)
		}
		if store.sessions[sid].Status != StatusActive || len(store.syncOps) != opsBefore {
			t.Errorf("a refused stop changed the session: status=%s ops=%v", store.sessions[sid].Status, store.ops())
		}
	})

	t.Run("the manifest's block gate holds with unreadable obligations", func(t *testing.T) {
		ev := screenEval()
		ev.Gate = "block"
		srv, _, sid := unreadable(t, ev, GateReview)
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrCompletenessUnchecked) {
			t.Errorf("Stop = %v; want ErrCompletenessUnchecked", err)
		}
	})

	t.Run("a human can force it, recorded as a decision", func(t *testing.T) {
		srv, store, sid := unreadable(t, screenEval(), GateBlock)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, true)
		if err != nil || res.Gate.Outcome != GateForced || !strings.Contains(res.Gate.Unchecked, "database is locked") {
			t.Fatalf("forced Stop = %+v, %v", res, err)
		}
		ds := store.decisionsFor(sid)
		if len(ds) != 1 || ds[0].ID != res.Gate.DecisionID || ds[0].Decision != "Completion forced by human without a completeness check" {
			t.Errorf("decision = %+v", ds)
		}
		comments := store.comments(t)
		if c := comments[len(comments)-1]; !strings.Contains(c, "**Completion gate (block):** completion forced by human; completeness couldn't be checked: stored obligations couldn't be read") {
			t.Errorf("finished comment = %q", c)
		}
	})

	t.Run("review moves the task to review with unreadable obligations", func(t *testing.T) {
		srv, store, sid := unreadable(t, screenEval(), GateReview)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Mode != GateReview || res.Gate.Outcome != GateMovedToReview || store.sessions[sid].Status != StatusCompleted {
			t.Fatalf("Stop = %+v, %v", res.Gate, err)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleReview}) {
			t.Errorf("roles = %v", roles)
		}
		comments := store.comments(t)
		if c := comments[len(comments)-1]; !strings.Contains(c, "**Completion gate (review):** completeness couldn't be checked, so the task goes to review instead of complete: stored obligations couldn't be read") {
			t.Errorf("finished comment = %q", c)
		}
	})

	t.Run("report completes with unreadable obligations", func(t *testing.T) {
		srv, store, sid := unreadable(t, screenEval(), GateReport)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Outcome != GateReported || res.Gate.Unchecked == "" || store.sessions[sid].Status != StatusCompleted {
			t.Errorf("Stop = %+v, %v", res.Gate, err)
		}
	})

	// failing starts a session whose evaluation fails at stop.
	failing := func(t *testing.T, gate GateMode) (*Service, *mockStore, string) {
		t.Helper()
		ev := &fakeEvaluator{eval: screenEval()}
		srv, store, dir := newCompletenessService(t, ev)
		srv.WithGate(gate)
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		ev.err = errors.New("permission denied")
		return srv, store, res.Session.ID
	}

	t.Run("a failed evaluation blocks under the configured block gate", func(t *testing.T) {
		srv, store, sid := failing(t, GateBlock)
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrCompletenessUnchecked) || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("Stop = %v; want ErrCompletenessUnchecked", err)
		}
		if store.sessions[sid].Status != StatusActive {
			t.Errorf("status = %s", store.sessions[sid].Status)
		}
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, true)
		if err != nil || res.Gate.Outcome != GateForced || len(store.decisionsFor(sid)) != 1 {
			t.Errorf("forced Stop = %+v, %v", res, err)
		}
	})

	t.Run("a failed evaluation moves the task to review under review", func(t *testing.T) {
		srv, store, sid := failing(t, GateReview)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Outcome != GateMovedToReview || !strings.Contains(res.Gate.Unchecked, "permission denied") || store.sessions[sid].Status != StatusCompleted {
			t.Fatalf("Stop = %+v, %v", res, err)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleReview}) {
			t.Errorf("roles = %v", roles)
		}
	})

	t.Run("a failed evaluation completes under report", func(t *testing.T) {
		srv, store, sid := failing(t, GateReport)
		res, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false)
		if err != nil || res.Gate.Outcome != GateReported || store.sessions[sid].Status != StatusCompleted {
			t.Errorf("Stop = %+v, %v", res, err)
		}
		if roles := store.statusRoles(t); !slices.Equal(roles, []StatusRole{RoleStart, RoleComplete}) {
			t.Errorf("roles = %v", roles)
		}
	})

	t.Run("pausing is never gated", func(t *testing.T) {
		srv, _, sid := failing(t, GateBlock)
		if _, err := srv.Stop(ctx, sid, "", "PAUSED", false, false); err != nil {
			t.Errorf("pause = %v", err)
		}
	})
}
