package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// attestedSuite is the suite obligation attested while command checks were
// off (attest_when: no_command asked for an attestation then).
func attestedSuite() *Obligation {
	o := stored("regression-suite", "suite", "web", ObligationAttested)
	o.VerifiedLevels = []string{levelAttest}
	o.Evidence = "ran pnpm test locally, all green"
	return o
}

func TestMergeObligations_AttestationNoLongerAsked(t *testing.T) {
	now := time.Now()

	t.Run("a failing command reopens an attestation the finding no longer asks for", func(t *testing.T) {
		o := attestedSuite()
		f := suiteFinding() // a command and no attest prompt: commands are on now
		run := failRun(*f.Command)
		run.Env = f.Command.envKey() // as runCommandChecks stamps it
		f.CommandResult = &run
		mr := mergeObligations("s", []*Obligation{o}, commandEval(f), now)
		if o.Status != ObligationOpen || slices.Contains(o.VerifiedLevels, levelAttest) || !mr.newlyOpen[o.ID] {
			t.Errorf("obligation = %+v; want open without the attest level", o)
		}
		st := &completenessState{eval: commandEval(f), obligations: mr.obligations, commands: mr.commands}
		if len(st.openRequired()) != 1 {
			t.Errorf("openRequired = %v; the gate must see it", st.openRequired())
		}
		// The old attestation isn't shown as standing evidence.
		if v := st.view(o, true); v.Evidence != "" {
			t.Errorf("view evidence = %q", v.Evidence)
		}
		if table := st.reportTable(); !strings.Contains(table, "`pnpm test`: failed (exit 1)") {
			t.Errorf("table = %s", table)
		}
	})

	t.Run("a command not run yet keeps it open", func(t *testing.T) {
		o := attestedSuite()
		mergeObligations("s", []*Obligation{o}, commandEval(suiteFinding()), now)
		if o.Status != ObligationOpen {
			t.Errorf("obligation = %+v", o)
		}
	})

	t.Run("a passing command satisfies it by its checks alone", func(t *testing.T) {
		o := attestedSuite()
		f := suiteFinding()
		pass := CommandCheck{Command: f.Command.Line, Root: f.Command.Root, Env: f.Command.envKey(), Passed: true}
		f.CommandResult = &pass
		mergeObligations("s", []*Obligation{o}, commandEval(f), now)
		if o.Status != ObligationSatisfied || !slices.Equal(o.VerifiedLevels, []string{levelCommand}) {
			t.Errorf("obligation = %+v; want satisfied with only the command level", o)
		}
	})

	t.Run("while the prompt stands the attestation stays sticky", func(t *testing.T) {
		o := attestedSuite()
		f := suiteFinding()
		f.AttestPrompt = "Which tests cover it?" // attest_when: always
		f.Satisfied = false
		run := failRun(*f.Command)
		f.CommandResult = &run
		mergeObligations("s", []*Obligation{o}, commandEval(f), now)
		if o.Status != ObligationAttested || !slices.Contains(o.VerifiedLevels, levelAttest) {
			t.Errorf("obligation = %+v", o)
		}
		pass := CommandCheck{Command: f.Command.Line, Root: f.Command.Root, Env: f.Command.envKey(), Passed: true}
		f.CommandResult = &pass
		mergeObligations("s", []*Obligation{o}, commandEval(f), now)
		if o.Status != ObligationSatisfied || !slices.Equal(o.VerifiedLevels, []string{levelCommand, levelAttest}) {
			t.Errorf("obligation = %+v", o)
		}
	})
}

func TestService_StopBlocksAttestedSuiteThatFails(t *testing.T) {
	ctx := context.Background()
	// Commands off: the requirement asks for an attestation instead.
	asked := suiteFinding()
	asked.Command = nil
	asked.AttestPrompt = "Did the suite pass?"
	asked.Satisfied = false
	ev := &fakeEvaluator{eval: evalWith(asked), run: failRun}
	srv, store, dir := newCompletenessService(t, ev)
	srv.WithGate(GateBlock)
	res, err := srv.Start(ctx, "task-1", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	sid := res.Session.ID
	if _, err := srv.Attest(ctx, sid, store.obligations[sid][0].ID, "ran it locally"); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	// allow_commands is turned on, and the suite fails.
	ev.eval = commandEval(suiteFinding())
	_, err = srv.Stop(ctx, sid, "", "COMPLETED", false, false)
	var inc *IncompleteSessionError
	if !errors.As(err, &inc) || len(inc.Open) != 1 || inc.Open[0].Command == nil {
		t.Fatalf("Stop = %v; want the failing suite to block", err)
	}
	if store.sessions[sid].Status != StatusActive {
		t.Errorf("session = %s", store.sessions[sid].Status)
	}
}

func TestService_VerifyRecordFailure(t *testing.T) {
	ctx := context.Background()
	store := newMockStore()
	failing := false
	hook := &upsertHook{mockStore: store, upsert: func(o *Obligation) error {
		if failing {
			return errors.New("database is locked")
		}
		return nil
	}}
	ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
	srv := NewService(hook, &mockGit{ctx: &GitContext{}}, nil).WithCompleteness(ev)
	res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	failing = true
	r, err := srv.Verify(ctx, res.Session.ID)
	if r != nil || err == nil {
		t.Fatalf("Verify = %+v, %v; want an error", r, err)
	}
	for _, want := range []string{"couldn't be recorded", "database is locked", "`pnpm test` in frontend: passed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want %q", err, want)
		}
	}
}

func TestService_CommandRunKeepsConcurrentChanges(t *testing.T) {
	ctx := context.Background()

	t.Run("a waiver made while the commands ran is kept", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
		srv, store, dir := newCompletenessService(t, ev)
		res, _ := srv.Start(ctx, "task-1", dir, false)
		sid := res.Session.ID
		id := store.obligations[sid][0].ID
		ev.run = func(spec CommandSpec) CommandCheck {
			if _, err := srv.Waive(ctx, sid, id, "suite is flaky upstream"); err != nil {
				t.Fatalf("Waive: %v", err)
			}
			return failRun(spec)
		}
		r, err := srv.Verify(ctx, sid)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		o := store.obligation(id)
		if o.Status != ObligationWaived || o.CommandCheck != nil {
			t.Errorf("stored = %+v; want the waiver kept and no run applied", o)
		}
		if r.Summary.Waived != 1 || r.Summary.Open != 0 {
			t.Errorf("summary = %+v", r.Summary)
		}
	})

	t.Run("a waiver made while a completing stop ran them lets it through", func(t *testing.T) {
		ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
		srv, store, dir := newCompletenessService(t, ev)
		srv.WithGate(GateBlock)
		res, _ := srv.Start(ctx, "task-1", dir, false)
		sid := res.Session.ID
		id := store.obligations[sid][0].ID
		ev.run = func(spec CommandSpec) CommandCheck {
			if _, err := srv.Waive(ctx, sid, id, "suite is flaky upstream"); err != nil {
				t.Fatalf("Waive: %v", err)
			}
			return failRun(spec)
		}
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if o := store.obligation(id); o.Status != ObligationWaived {
			t.Errorf("stored = %+v", o)
		}
	})

	t.Run("a failed re-read leaves completeness unchecked", func(t *testing.T) {
		store := newMockStore()
		read := func(id string) ([]*Obligation, error) { return store.ListObligations(ctx, id) }
		hook := &listHook{mockStore: store, list: read}
		ev := &fakeEvaluator{eval: commandEval(suiteFinding())}
		srv := NewService(hook, &mockGit{ctx: &GitContext{}}, nil).WithCompleteness(ev)
		res, err := srv.Start(ctx, "task-1", t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		sid := res.Session.ID
		ev.run = func(spec CommandSpec) CommandCheck {
			hook.list = func(string) ([]*Obligation, error) { return nil, errors.New("database is locked") }
			return CommandCheck{Command: spec.Line, Root: spec.Root, Passed: true, RanAt: time.Now()}
		}
		if _, err := srv.Verify(ctx, sid); !errors.Is(err, ErrCompletenessUnchecked) {
			t.Errorf("Verify err = %v; want ErrCompletenessUnchecked", err)
		}
		if o := store.obligations[sid][0]; o.Status != ObligationOpen || o.CommandCheck != nil {
			t.Errorf("stored = %+v; nothing should be written", o)
		}
	})
}

func TestService_CommandRunsByEnv(t *testing.T) {
	ctx := context.Background()
	web := finding("web-suite", "suite", "web", true)
	web.ChecksPassed = true
	web.Command = &CommandSpec{Line: "make test", Root: ".", Env: map[string]string{"TARGET": "web"}, Fingerprint: "state-web"}
	api := finding("api-suite", "suite", "api", true)
	api.ChecksPassed = true
	api.Command = &CommandSpec{Line: "make test", Root: ".", Env: map[string]string{"TARGET": "api"}, Fingerprint: "state-api"}
	ev := &fakeEvaluator{eval: commandEval(web, api), run: func(spec CommandSpec) CommandCheck {
		c := CommandCheck{Command: spec.Line, Root: spec.Root, Passed: true, RanAt: time.Now()}
		if spec.Env["TARGET"] == "api" {
			c = failRun(spec)
		}
		return c
	}}
	srv, store, dir := newCompletenessService(t, ev)
	res, _ := srv.Start(ctx, "task-1", dir, false)
	sid := res.Session.ID

	r, err := srv.Verify(ctx, sid)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(ev.ran) != 2 || len(r.Commands) != 2 {
		t.Fatalf("ran = %+v; want one run per env", ev.ran)
	}
	for _, o := range store.obligations[sid] {
		want := ObligationSatisfied
		if o.RuleID == "api-suite" {
			want = ObligationOpen
		}
		if o.Status != want {
			t.Errorf("%s = %s, want %s (%+v)", o.RuleID, o.Status, want, o.CommandCheck)
		}
	}
}

func TestCommandCheck_RunsMatchesEnv(t *testing.T) {
	a := &CommandSpec{Line: "make test", Root: ".", Env: map[string]string{"TARGET": "web"}, Fingerprint: "same"}
	b := &CommandSpec{Line: "make test", Root: ".", Env: map[string]string{"TARGET": "api"}, Fingerprint: "same"}
	c := &CommandCheck{Command: "make test", Root: ".", Env: a.envKey(), Passed: true, Fingerprint: "same"}
	if !c.current(a) || c.runs(b) || c.current(b) {
		t.Errorf("a run with env %v must count only for that env", a.Env)
	}
	if (&CommandSpec{}).envKey() != "" {
		t.Error("no env should have an empty key")
	}
	if a.envKey() != (&CommandSpec{Env: map[string]string{"TARGET": "web"}}).envKey() || strings.Contains(a.envKey(), "web") {
		t.Errorf("env key %q should be a stable digest", a.envKey())
	}
}
