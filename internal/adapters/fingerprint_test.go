package adapters

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/gitctx"
	"github.com/b1codes/taskflow/internal/session"
)

const scriptManifest = `version: 1
stacks:
  - { id: app, root: app, env: { CI: "true" }, commands: { test: [{ run: "./t.sh" }] } }
rules:
  - id: suite
    stack: app
    describe: "suite"
    scope: stack
    when: { changed: "**" }
    require: [{ id: suite, describe: "suite passes", run: { command: test } }]
`

// fingerprintRepo makes a committed repository whose app stack's test
// command is ./t.sh (script), with app/main.go changed since base, and
// *.log and .env ignored.
func fingerprintRepo(t *testing.T, script string) (dir string, sess *session.Session, write func(rel, content string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write = func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@test.com")
	git("config", "commit.gpgsign", "false")
	write(".gitignore", "*.log\n.env\n")
	write(".agents/completeness.yaml", scriptManifest)
	write("app/t.sh", "#!/bin/sh\n"+script+"\n")
	write("app/main.go", "package main")
	write(".env", "SECRET=a")
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	base, err := NewCompletenessEvaluator().BaseSHA(dir)
	if err != nil {
		t.Fatal(err)
	}
	write("app/main.go", "package main // changed")
	return dir, &session.Session{ID: "s", ProjectPath: dir, BaseSHA: base}, write
}

// evalSpec evaluates with stored and returns the one command spec.
func evalSpec(t *testing.T, e *CompletenessEvaluator, sess *session.Session, stored []*session.Obligation) (*session.CommandSpec, []string) {
	t.Helper()
	ev, err := e.Evaluate(context.Background(), sess, stored)
	if err != nil || ev == nil || ev.Error != "" || len(ev.Findings) != 1 || ev.Findings[0].Command == nil {
		t.Fatalf("Evaluate = %+v, %v", ev, err)
	}
	return ev.Findings[0].Command, ev.Warnings
}

// runOnce runs the stack's command through the evaluator and returns its
// check, which must pass and carry a fingerprint.
func runOnce(t *testing.T, e *CompletenessEvaluator, sess *session.Session) session.CommandCheck {
	t.Helper()
	spec, _ := evalSpec(t, e, sess, nil)
	checks := e.RunCommands(context.Background(), sess, []session.CommandSpec{*spec})
	if len(checks) != 1 || !checks[0].Passed || checks[0].Fingerprint == "" {
		t.Fatalf("RunCommands = %+v", checks)
	}
	return checks[0]
}

func storedPass(c session.CommandCheck) []*session.Obligation {
	return []*session.Obligation{{ID: "o", RuleID: "suite", RequirementID: "suite", Subject: "app", Status: session.ObligationSatisfied, CommandCheck: &c}}
}

// A test run that writes ignored files (logs, coverage, caches) leaves its
// own pass current: the run is stamped with the state it left behind. An
// ignored file changed afterwards still makes it stale.
func TestRunCommands_IgnoredOutputKeepsPassCurrent(t *testing.T) {
	t.Parallel()
	_, sess, write := fingerprintRepo(t, "echo run >> test.log")
	e := NewCompletenessEvaluator().WithCommands(true, time.Minute)

	check := runOnce(t, e, sess)
	if spec, _ := evalSpec(t, e, sess, storedPass(check)); spec.Fingerprint != check.Fingerprint {
		t.Fatalf("after the run: spec fingerprint %q, run's %q: the pass should be current", spec.Fingerprint, check.Fingerprint)
	}
	// Running again appends to the log again: still current afterwards.
	again := runOnce(t, e, sess)
	if again.Fingerprint == check.Fingerprint {
		t.Error("the log grew, so the state the run left differs")
	}
	if spec, _ := evalSpec(t, e, sess, storedPass(again)); spec.Fingerprint != again.Fingerprint {
		t.Error("a second run should be current too")
	}

	write(".env", "SECRET=b")
	if spec, _ := evalSpec(t, e, sess, storedPass(again)); spec.Fingerprint == again.Fingerprint {
		t.Error("an ignored file changed after the run should make the pass stale")
	}
}

// A change to the content (here a tracked file, by the run itself) while
// the commands ran stamps the pre-run state, so the pass is stale.
func TestRunCommands_ContentChangeDuringRunIsStale(t *testing.T) {
	t.Parallel()
	_, sess, _ := fingerprintRepo(t, "echo '// edited' >> main.go")
	e := NewCompletenessEvaluator().WithCommands(true, time.Minute)

	before, _ := evalSpec(t, e, sess, storedPass(session.CommandCheck{Command: "./t.sh", Root: "app", Passed: true, Fingerprint: "earlier"}))
	check := runOnce(t, e, sess)
	if check.Fingerprint != before.Fingerprint {
		t.Errorf("run fingerprint %q, want the pre-run state's %q", check.Fingerprint, before.Fingerprint)
	}
	if spec, _ := evalSpec(t, e, sess, storedPass(check)); spec.Fingerprint == check.Fingerprint {
		t.Error("a tracked file edited during the run should leave the pass stale")
	}
}

// RunCommands hashes the work tree before and after the batch, and not at
// all for no specs; without a pre-run hash the runs are left unstamped.
func TestRunCommands_Stamping(t *testing.T) {
	t.Parallel()
	_, sess, _ := fingerprintRepo(t, "true")
	calls := 0
	states := []gitctx.WorkTreeState{{Content: "c", Ignored: "i1"}, {Content: "c", Ignored: "i2"}}
	e := NewCompletenessEvaluator().WithCommands(true, time.Minute)
	e.hashTree = func(string) (gitctx.WorkTreeState, error) {
		s := states[calls%len(states)]
		calls++
		return s, nil
	}
	specs := []session.CommandSpec{{Line: "true", Root: "app"}, {Line: "true", Root: "app", Env: map[string]string{"CI": "1"}}}
	checks := e.RunCommands(context.Background(), sess, specs)
	if calls != 2 {
		t.Errorf("hashed %d times, want 2 (before and after)", calls)
	}
	after := states[1].Hash()
	if checks[0].Fingerprint != commandFingerprint(after, nil) || checks[1].Fingerprint != commandFingerprint(after, specs[1].Env) {
		t.Errorf("runs should carry the after state with their own env: %+v", checks)
	}

	calls = 0
	if e.RunCommands(context.Background(), sess, nil); calls != 0 {
		t.Errorf("no specs: hashed %d times", calls)
	}

	e.hashTree = func(string) (gitctx.WorkTreeState, error) { return gitctx.WorkTreeState{}, errors.New("boom") }
	if checks := e.RunCommands(context.Background(), sess, specs[:1]); checks[0].Fingerprint != "" {
		t.Errorf("without a pre-run hash: fingerprint %q", checks[0].Fingerprint)
	}
}

// Evaluate hashes the work tree only when a stored pass for the same
// command could be current, once per evaluation.
func TestEvaluate_HashesOnlyForStoredPass(t *testing.T) {
	t.Parallel()
	_, sess, _ := fingerprintRepo(t, "true")
	calls := 0
	e := NewCompletenessEvaluator().WithCommands(true, time.Minute)
	e.hashTree = func(string) (gitctx.WorkTreeState, error) {
		calls++
		return gitctx.WorkTreeState{Content: "c", Ignored: "i"}, nil
	}
	pass := session.CommandCheck{Command: "./t.sh", Root: "app", Passed: true, Fingerprint: "earlier"}
	failed, otherLine, otherRoot, unstamped := pass, pass, pass, pass
	failed.Passed = false
	otherLine.Command = "make test"
	otherRoot.Root = "web"
	unstamped.Fingerprint = ""
	for _, tc := range []struct {
		name   string
		stored []*session.Obligation
	}{
		{"nothing stored", nil},
		{"a failed run", storedPass(failed)},
		{"a pass of another command line", storedPass(otherLine)},
		{"a pass in another root", storedPass(otherRoot)},
		{"a pass without a fingerprint", storedPass(unstamped)},
	} {
		calls = 0
		if spec, _ := evalSpec(t, e, sess, tc.stored); spec.Fingerprint != "" || calls != 0 {
			t.Errorf("%s: fingerprint %q, %d hashes; want none", tc.name, spec.Fingerprint, calls)
		}
	}

	calls = 0
	spec, _ := evalSpec(t, e, sess, storedPass(pass))
	if want := commandFingerprint(gitctx.WorkTreeState{Content: "c", Ignored: "i"}.Hash(), map[string]string{"CI": "true"}); spec.Fingerprint != want || calls != 1 {
		t.Errorf("with a stored pass: fingerprint %q (want %q), %d hashes", spec.Fingerprint, want, calls)
	}

	// A failed hash warns and leaves no fingerprint.
	e.hashTree = func(string) (gitctx.WorkTreeState, error) { return gitctx.WorkTreeState{}, errors.New("boom") }
	spec, warnings := evalSpec(t, e, sess, storedPass(pass))
	if spec.Fingerprint != "" || !strings.Contains(strings.Join(warnings, "\n"), "can't be tied to the project's state") {
		t.Errorf("failed hash: fingerprint %q, warnings %v", spec.Fingerprint, warnings)
	}
}
