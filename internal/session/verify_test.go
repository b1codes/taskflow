package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// suiteFinding is a stack-scoped regression suite checked by a command.
func suiteFinding() CompletenessFinding {
	f := finding("regression-suite", "suite", "web", true)
	f.Levels = nil
	f.ChecksPassed = true
	f.Command = &CommandSpec{Line: "pnpm test", Root: "frontend", Env: map[string]string{"CI": "true"}, Fingerprint: "state-1"}
	return f
}

// suiteEnv is suiteFinding's env as a run records it.
var suiteEnv = (&CommandSpec{Env: map[string]string{"CI": "true"}}).envKey()

func commandEval(findings ...CompletenessFinding) *Evaluation {
	ev := evalWith(findings...)
	ev.CommandsEnabled = true
	return ev
}

func failRun(spec CommandSpec) CommandCheck {
	return CommandCheck{Command: spec.Line, Root: spec.Root, ExitCode: 1, Output: "FAIL src/a.test.ts\nTests: 1 failed, 9 passed", RanAt: time.Now()}
}

func TestMergeObligations_CommandChecks(t *testing.T) {
	now := time.Now()

	t.Run("a command not yet run keeps the obligation open", func(t *testing.T) {
		mr := mergeObligations("s", nil, commandEval(suiteFinding()), now)
		o := mr.obligations[0]
		if o.Status != ObligationOpen || len(o.VerifiedLevels) != 0 || mr.commands[o.ID] == nil {
			t.Errorf("obligation = %+v, command = %+v", o, mr.commands[o.ID])
		}
	})

	t.Run("a passing run satisfies it and adds the command level", func(t *testing.T) {
		f := suiteFinding()
		f.CommandResult = &CommandCheck{Command: "pnpm test", Root: "frontend", Env: suiteEnv, Passed: true}
		mr := mergeObligations("s", nil, commandEval(f), now)
		o := mr.obligations[0]
		if o.Status != ObligationSatisfied || !slices.Equal(o.VerifiedLevels, []string{levelCommand}) || o.CommandCheck == nil {
			t.Errorf("obligation = %+v", o)
		}
	})

	t.Run("a stored pass counts only while the project is as it ran", func(t *testing.T) {
		pass := func(fingerprint string) *Obligation {
			o := stored("regression-suite", "suite", "web", ObligationSatisfied)
			o.VerifiedLevels = []string{levelCommand}
			o.CommandCheck = &CommandCheck{Command: "pnpm test", Root: "frontend", Env: suiteEnv, Passed: true, Fingerprint: fingerprint}
			return o
		}
		o := pass("state-1")
		mergeObligations("s", []*Obligation{o}, commandEval(suiteFinding()), now)
		if o.Status != ObligationSatisfied {
			t.Errorf("a current pass should stand: %+v", o)
		}

		changed := suiteFinding()
		changed.Command.Fingerprint = "state-2" // the code changed after the run
		o = pass("state-1")
		mr := mergeObligations("s", []*Obligation{o}, commandEval(changed), now)
		if o.Status != ObligationOpen || slices.Contains(o.VerifiedLevels, levelCommand) || !mr.newlyOpen[o.ID] {
			t.Errorf("a pass from before a change shouldn't count: %+v", o)
		}

		// Without a fingerprint (no git) a stored pass can't be trusted.
		unknown := suiteFinding()
		unknown.Command.Fingerprint = ""
		o = pass("")
		mergeObligations("s", []*Obligation{o}, commandEval(unknown), now)
		if o.Status != ObligationOpen {
			t.Errorf("a pass with no fingerprint shouldn't count later: %+v", o)
		}

		other := suiteFinding()
		other.Command.Line = "pnpm run test:ci"
		o = pass("state-1")
		mergeObligations("s", []*Obligation{o}, commandEval(other), now)
		if o.Status != ObligationOpen {
			t.Errorf("a pass of another command line shouldn't count: %+v", o)
		}
	})

	t.Run("a fresh pass counts without a fingerprint", func(t *testing.T) {
		f := suiteFinding()
		f.Command.Fingerprint = ""
		f.CommandResult = &CommandCheck{Command: "pnpm test", Root: "frontend", Env: suiteEnv, Passed: true}
		mr := mergeObligations("s", nil, commandEval(f), now)
		if o := mr.obligations[0]; o.Status != ObligationSatisfied {
			t.Errorf("obligation = %+v", o)
		}
	})

	t.Run("a failing run reopens it", func(t *testing.T) {
		o := stored("regression-suite", "suite", "web", ObligationSatisfied)
		o.CommandCheck = &CommandCheck{Command: "pnpm test", Root: "frontend", Env: suiteEnv, Passed: true}
		f := suiteFinding()
		run := failRun(*f.Command)
		f.CommandResult = &run
		mr := mergeObligations("s", []*Obligation{o}, commandEval(f), now)
		if o.Status != ObligationOpen || o.CommandCheck.Passed || !mr.newlyOpen[o.ID] {
			t.Errorf("obligation = %+v", o)
		}
	})

	t.Run("a failing structural check isn't rescued by the command", func(t *testing.T) {
		f := suiteFinding()
		f.Satisfied, f.ChecksPassed = false, false
		f.CommandResult = &CommandCheck{Command: "pnpm test", Root: "frontend", Env: suiteEnv, Passed: true}
		mr := mergeObligations("s", nil, commandEval(f), now)
		if o := mr.obligations[0]; o.Status != ObligationOpen {
			t.Errorf("obligation = %+v", o)
		}
	})
}

func TestService_Verify(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, ev *fakeEvaluator) (*Service, *mockStore, string) {
		t.Helper()
		srv, store, dir := newCompletenessService(t, ev)
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return srv, store, res.Session.ID
	}

	t.Run("runs each command once and records the results", func(t *testing.T) {
		contract := finding("react-screen", "contract", "src/A.tsx", true)
		contract.ChecksPassed = true
		contract.Command = &CommandSpec{Line: "pnpm test", Root: "frontend", Env: map[string]string{"CI": "true"}, Fingerprint: "state-1"} // same run as the suite
		ev := &fakeEvaluator{eval: commandEval(suiteFinding(), contract)}
		srv, store, sid := start(t, ev)

		r, err := srv.Verify(ctx, sid)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(ev.ran) != 1 || ev.ran[0].Env["CI"] != "true" {
			t.Errorf("ran = %+v, want one run with the stack env", ev.ran)
		}
		if len(r.Commands) != 1 || !r.Commands[0].Passed || r.Summary.Satisfied != 2 || r.Summary.Open != 0 {
			t.Errorf("report = %+v", r)
		}
		for _, o := range store.obligations[sid] {
			if o.Status != ObligationSatisfied || o.CommandCheck == nil || !slices.Contains(o.VerifiedLevels, levelCommand) {
				t.Errorf("stored = %+v", o)
			}
		}
		if v := r.Obligations[0].Command; v == nil || v.Result != "passed" || v.Output != "" {
			t.Errorf("command view = %+v", v)
		}
		for _, o := range store.obligations[sid] {
			if o.CommandCheck.Fingerprint != "state-1" {
				t.Errorf("the run should be tied to the state it ran against: %+v", o.CommandCheck)
			}
		}

		// A later checkpoint doesn't run anything, and the pass stands while
		// nothing changed.
		cp, err := srv.Checkpoint(ctx, sid, "more", nil, false)
		if err != nil || cp.Completeness.Summary.Open != 0 || len(ev.ran) != 1 {
			t.Errorf("checkpoint = %+v, %v, ran %d", cp.Completeness, err, len(ev.ran))
		}

		// Once the project changes, it doesn't: Check blocks and says why.
		for i := range ev.eval.Findings {
			ev.eval.Findings[i].Command.Fingerprint = "state-2"
		}
		res, err := srv.Check(ctx, sid, false)
		if err != nil || !res.Blocked || len(res.OpenRequired) != 2 {
			t.Fatalf("check after a change = %+v, %v", res, err)
		}
		if v := res.OpenRequired[0].Command; v == nil || !strings.HasPrefix(v.Result, "stale:") {
			t.Errorf("command view = %+v", v)
		}
		if !strings.Contains(res.Reason(), "stale: it passed, but the project changed since; run tf_verify again") {
			t.Errorf("reason = %s", res.Reason())
		}
		if len(ev.ran) != 1 {
			t.Errorf("check must not run commands: ran %d", len(ev.ran))
		}
	})

	t.Run("a failure is open with its output", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding()), run: failRun}
		srv, _, sid := start(t, ev)
		r, err := srv.Verify(ctx, sid)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		v := r.Obligations[0]
		if v.Status != ObligationOpen || v.Command == nil || v.Command.Result != "failed (exit 1)" || !strings.Contains(v.Command.Output, "1 failed") {
			t.Errorf("view = %+v / %+v", v, v.Command)
		}
	})

	t.Run("waived obligations aren't run", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
		srv, store, sid := start(t, ev)
		if _, err := srv.Waive(ctx, sid, store.obligations[sid][0].ID, "suite is flaky upstream"); err != nil {
			t.Fatal(err)
		}
		r, err := srv.Verify(ctx, sid)
		if err != nil || len(ev.ran) != 0 || len(r.Warnings) == 0 {
			t.Errorf("Verify = %+v, %v; ran %v", r, err, ev.ran)
		}
	})

	t.Run("refused while commands are disabled", func(t *testing.T) {
		srv, _, sid := start(t, &fakeEvaluator{eval: evalWith(finding("r", "q", "A", false))})
		if _, err := srv.Verify(ctx, sid); !errors.Is(err, ErrCommandsDisabled) {
			t.Errorf("err = %v, want ErrCommandsDisabled", err)
		}
	})

	t.Run("refused without a manifest, or when it is invalid", func(t *testing.T) {
		srv, _, sid := start(t, &fakeEvaluator{})
		if _, err := srv.Verify(ctx, sid); err == nil {
			t.Error("Verify without a manifest should fail")
		}
		srv, _, sid = start(t, &fakeEvaluator{eval: &Evaluation{Manifest: "m", Error: "bad", CommandsEnabled: true}})
		if _, err := srv.Verify(ctx, sid); !errors.Is(err, ErrCompletenessUnchecked) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestService_StopRunsCommands(t *testing.T) {
	ctx := context.Background()

	t.Run("completing runs them and the gate sees the result", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding()), run: failRun}
		srv, store, dir := newCompletenessService(t, ev)
		res, _ := srv.Start(ctx, "task-1", dir, false)
		stop, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(ev.ran) != 1 || stop.Gate.Outcome != GateMovedToReview {
			t.Errorf("ran = %v, gate = %+v", ev.ran, stop.Gate)
		}
		comments := store.comments(t)
		if c := comments[len(comments)-1]; !strings.Contains(c, "`pnpm test`: failed (exit 1): Tests: 1 failed, 9 passed") {
			t.Errorf("finished comment = %q", c)
		}
	})

	t.Run("a blocked stop reports the failure and writes nothing", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding()), run: failRun}
		srv, store, dir := newCompletenessService(t, ev)
		srv.WithGate(GateBlock)
		res, _ := srv.Start(ctx, "task-1", dir, false)
		before := *store.obligations[res.Session.ID][0]
		_, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false)
		var inc *IncompleteSessionError
		if !errors.As(err, &inc) || inc.Open[0].Command == nil || !strings.Contains(inc.Open[0].Command.Output, "1 failed") {
			t.Fatalf("err = %v", err)
		}
		if after := store.obligations[res.Session.ID][0]; after.CommandCheck != nil || after.Status != before.Status {
			t.Errorf("a blocked stop wrote the run: %+v", after)
		}
	})

	t.Run("pausing doesn't run them", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
		srv, _, dir := newCompletenessService(t, ev)
		res, _ := srv.Start(ctx, "task-1", dir, false)
		if _, err := srv.Stop(ctx, res.Session.ID, "", "PAUSED", false, false); err != nil || len(ev.ran) != 0 {
			t.Errorf("err = %v, ran = %v", err, ev.ran)
		}
	})
}

func TestService_AttestRefusesCommandChecks(t *testing.T) {
	ctx := context.Background()
	srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: commandEval(suiteFinding())})
	res, _ := srv.Start(ctx, "task-1", dir, false)
	_, err := srv.Attest(ctx, res.Session.ID, store.obligations[res.Session.ID][0].ID, "ran it, all green")
	if !errors.Is(err, ErrNotAttestable) || !strings.Contains(err.Error(), "tf_verify") {
		t.Errorf("err = %v", err)
	}
}

func TestChecklistItem_CommandLevel(t *testing.T) {
	o := stored("regression-suite", "suite", "web", ObligationSatisfied)
	o.VerifiedLevels = []string{"structural", levelCommand}
	it := checklistItem(o)
	if !strings.HasSuffix(it.Name, " — structural, command ✓") || !ChecklistItemMatches(it.Name, it.Match) {
		t.Errorf("item = %+v", it)
	}
}
