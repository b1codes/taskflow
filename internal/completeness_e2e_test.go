package internal_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/adapters"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

// handWrittenManifest is a .taskflow/completeness.yaml written without b1.
const handWrittenManifest = `version: 1
stacks:
  - id: web
    root: "."
    vars:
      pm:
        - { if: { exists: "pnpm-lock.yaml" }, value: "pnpm" }
        - { value: "npm" }
    commands:
      test:
        - { if: { has_script: "test:ci" }, run: "{pm} run test:ci" }
        - { if: { has_script: "test" }, run: "{pm} test" }
rules:
  - id: react-screen
    stack: web
    describe: "New screen"
    when:
      added: "src/features/{feature}/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx", "**/*.test.tsx"]
    require:
      - id: story
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.stories.tsx"
          contains: ["export default", "{name}"]
      - id: unit-test
        describe: "Component test for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.test.tsx"
  - id: regression-suite
    stack: web
    describe: "Existing {stack} test suite still passes"
    when: { changed: "src/**" }
    scope: stack
    require:
      - id: suite
        describe: "Full {stack} test suite passes"
        run: { command: test }
        attest: "Run ` + "`{command}`" + ` in ` + "`{root}`" + ` and paste the final summary line"
        attest_when: no_command
`

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func describes(views []session.ObligationView) []string {
	var out []string
	for _, v := range views {
		out = append(out, v.Describe)
	}
	slices.Sort(out)
	return out
}

func TestCompleteness_EndToEnd_ReactScreen(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping: git init failed")
	}
	gitRun(t, dir, "config", "user.name", "Test")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "package.json", `{"scripts": {"test": "vitest run"}}`)
	writeFile(t, dir, ".taskflow/completeness.yaml", handWrittenManifest)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "initial")

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	srv := session.NewService(st, adapters.NewGitCapturer(), nil).WithCompleteness(adapters.NewCompletenessEvaluator())
	ctx := context.Background()

	// 1. Start: the contract carries the resolved stack.
	start, err := srv.Start(ctx, "task-e2e", dir, true)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := start.Session.ID
	cc := start.AgenticContract.Completeness
	if cc == nil || cc.Manifest != ".taskflow/completeness.yaml" || cc.Error != "" {
		t.Fatalf("contract completeness = %+v", cc)
	}
	if len(cc.Stacks) != 1 || cc.Stacks[0].Commands["test"] == nil || *cc.Stacks[0].Commands["test"] != "npm test" {
		t.Fatalf("stacks = %+v", cc.Stacks)
	}

	// 2. Add a screen: its requirements and the regression suite open.
	writeFile(t, dir, "src/features/payments/pages/CheckoutPage.tsx", "export function CheckoutPage() {}")
	cp, err := srv.Checkpoint(ctx, sid, "scaffolded CheckoutPage", nil, true)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c := cp.Completeness
	want := []string{"Component test for CheckoutPage", "Full web test suite passes", "Storybook stories for CheckoutPage"}
	if c == nil || !slices.Equal(describes(c.NewlyOpen), want) {
		t.Fatalf("newly_open = %+v", c)
	}
	for _, v := range c.NewlyOpen {
		if v.Rule == "regression-suite" && v.Attest != "Run `npm test` in `.` and paste the final summary line" {
			t.Errorf("regression attest prompt = %q", v.Attest)
		}
		if v.Requirement == "story" && v.Expected != "src/features/payments/pages/CheckoutPage.stories.tsx" {
			t.Errorf("story expected = %q", v.Expected)
		}
	}

	// 3. Commit the screen, then add its story and test: committed work is
	// still part of the session's changes, and both obligations are met.
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "screen")
	writeFile(t, dir, "src/features/payments/pages/CheckoutPage.stories.tsx", "export default { component: CheckoutPage }")
	writeFile(t, dir, "src/features/payments/pages/CheckoutPage.test.tsx", "test('renders')")
	// A second screen that gets reverted before the next checkpoint.
	writeFile(t, dir, "src/features/payments/pages/RefundPage.tsx", "x")
	cp, err = srv.Checkpoint(ctx, sid, "stories and tests", nil, true)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c = cp.Completeness
	if !slices.Equal(c.NewlySatisfied, []string{"Storybook stories for CheckoutPage", "Component test for CheckoutPage"}) {
		t.Errorf("newly_satisfied = %v", c.NewlySatisfied)
	}
	if !slices.Equal(c.StillOpen, []string{"Full web test suite passes"}) {
		t.Errorf("still_open = %v", c.StillOpen)
	}
	if got := describes(c.NewlyOpen); !slices.Equal(got, []string{"Component test for RefundPage", "Storybook stories for RefundPage"}) {
		t.Errorf("newly_open = %v", got)
	}

	if err := os.Remove(filepath.Join(dir, "src/features/payments/pages/RefundPage.tsx")); err != nil {
		t.Fatal(err)
	}
	report, err := srv.Completeness(ctx, sid)
	if err != nil {
		t.Fatalf("Completeness: %v", err)
	}
	if report.Summary != (session.CompletenessSummary{Open: 1, Satisfied: 2}) || len(report.Obligations) != 3 {
		t.Errorf("after reverting RefundPage: summary=%+v obligations=%+v", report.Summary, report.Obligations)
	}

	// 4. Stop: the finished comment carries the completeness table.
	if _, err := srv.Stop(ctx, sid, "CheckoutPage done", "COMPLETED", true, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ops, err := st.DequeuePending(ctx, 100)
	if err != nil {
		t.Fatalf("DequeuePending: %v", err)
	}
	var comments []string
	for _, op := range ops {
		if op.Operation != "POST_COMMENT" {
			continue
		}
		var p struct {
			CommentText string `json:"comment_text"`
		}
		_ = json.Unmarshal([]byte(op.Payload), &p)
		comments = append(comments, p.CommentText)
	}
	if len(comments) != 4 {
		t.Fatalf("expected 3 checkpoint comments + finished comment, got %d", len(comments))
	}
	if !strings.HasSuffix(comments[0], "**Completeness:** 0/3 · open: story, unit-test, suite") {
		t.Errorf("first checkpoint line = %q", comments[0][strings.LastIndex(comments[0], "\n")+1:])
	}
	if strings.Contains(comments[2], "Completeness") {
		t.Errorf("stop checkpoint repeats the finished comment's table: %q", comments[2])
	}
	finished := comments[3]
	for _, want := range []string{
		"### Completeness (2/3)",
		"| Full web test suite passes | `web` | required | open | — | — |",
		"| Storybook stories for CheckoutPage | `src/features/payments/pages/CheckoutPage.tsx` | required | satisfied | structural ✓ | — |",
	} {
		if !strings.Contains(finished, want) {
			t.Errorf("finished comment missing %q:\n%s", want, finished)
		}
	}
	if strings.Contains(finished, "RefundPage") {
		t.Errorf("void obligations must not be reported:\n%s", finished)
	}
}

func TestCompleteness_EndToEnd_NoGit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".taskflow/completeness.yaml", handWrittenManifest)
	writeFile(t, dir, "src/features/a/pages/X.tsx", "x")

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	srv := session.NewService(st, adapters.NewGitCapturer(), nil).WithCompleteness(adapters.NewCompletenessEvaluator())
	ctx := context.Background()

	start, err := srv.Start(ctx, "task-nogit", dir, true)
	if err != nil {
		t.Fatalf("Start must succeed without git: %v", err)
	}
	cp, err := srv.Checkpoint(ctx, start.Session.ID, "work", nil, true)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c := cp.Completeness
	if c == nil || len(c.NewlyOpen) != 0 || len(c.Warnings) == 0 || !strings.Contains(c.Warnings[0], "not a git repository") {
		t.Errorf("no-git checkpoint = %+v", c)
	}
}

// newE2ERepo creates a git repo holding manifest, committed, and a service
// over an in-memory store.
func newE2ERepo(t *testing.T, manifest string) (string, store.Store, *session.Service) {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping: git init failed")
	}
	gitRun(t, dir, "config", "user.name", "Test")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "package.json", `{"scripts": {"test": "vitest run"}}`)
	writeFile(t, dir, ".taskflow/completeness.yaml", manifest)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "initial")

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st, session.NewService(st, adapters.NewGitCapturer(), nil).WithCompleteness(adapters.NewCompletenessEvaluator())
}

// queuedOps returns the pending sync ops by operation.
func queuedOps(t *testing.T, st store.Store) map[string][]string {
	t.Helper()
	ops, err := st.DequeuePending(context.Background(), 100)
	if err != nil {
		t.Fatalf("DequeuePending: %v", err)
	}
	out := map[string][]string{}
	for _, op := range ops {
		out[op.Operation] = append(out[op.Operation], op.Payload)
	}
	return out
}

func obligationByRequirement(t *testing.T, views []session.ObligationView, requirement string) session.ObligationView {
	t.Helper()
	for _, v := range views {
		if v.Requirement == requirement {
			return v
		}
	}
	t.Fatalf("no %s obligation in %+v", requirement, views)
	return session.ObligationView{}
}

func TestCompleteness_EndToEnd_PlanWaiveStop(t *testing.T) {
	manifest := "gate: block\n" + strings.Replace(handWrittenManifest, "    describe: \"New screen\"\n", "    describe: \"New screen\"\n    plannable: true\n", 1)
	dir, st, srv := newE2ERepo(t, manifest)
	ctx := context.Background()

	start, err := srv.Start(ctx, "task-enforce", dir, true)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := start.Session.ID
	if cc := start.AgenticContract.Completeness; cc.Gate != session.GateBlock || len(cc.Rules) != 2 || !cc.Rules[0].Plannable {
		t.Fatalf("contract = %+v", cc)
	}

	// 1. Plan the screen before it exists.
	plan, err := srv.Plan(ctx, sid, []session.PlanItem{{RuleID: "react-screen", Vars: map[string]string{"feature": "payments", "name": "CheckoutPage"}}}, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := describes(plan.Planned); !slices.Equal(got, []string{"Component test for CheckoutPage", "New screen: CheckoutPage.tsx", "Storybook stories for CheckoutPage"}) {
		t.Fatalf("planned = %v", got)
	}
	if _, err := srv.Plan(ctx, sid, []session.PlanItem{{RuleID: "react-screen", Vars: map[string]string{"name": "X"}}}, ""); !errors.Is(err, session.ErrInvalidPlan) {
		t.Errorf("a plan missing vars = %v", err)
	}

	// 2. The block gate refuses to complete while the plan is undone.
	if _, err := srv.Stop(ctx, sid, "", "COMPLETED", true, false); !errors.Is(err, session.ErrIncompleteSession) {
		t.Fatalf("Stop with planned work open = %v", err)
	}

	// 3. Build the screen and its story; the planned obligations follow.
	writeFile(t, dir, "src/features/payments/pages/CheckoutPage.tsx", "export function CheckoutPage() {}")
	writeFile(t, dir, "src/features/payments/pages/CheckoutPage.stories.tsx", "export default { component: CheckoutPage }")
	cp, err := srv.Checkpoint(ctx, sid, "screen and story", nil, true)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := slices.Sorted(slices.Values(cp.Completeness.NewlySatisfied)); !slices.Equal(got, []string{"New screen: CheckoutPage.tsx", "Storybook stories for CheckoutPage"}) {
		t.Errorf("newly_satisfied = %v", got)
	}
	report, err := srv.Completeness(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary != (session.CompletenessSummary{Open: 2, Satisfied: 2}) {
		t.Fatalf("summary = %+v (%+v)", report.Summary, report.Obligations)
	}

	// 4. Waive the unit test and attest the suite.
	unit := obligationByRequirement(t, report.Obligations, "unit-test")
	waived, err := srv.Waive(ctx, sid, unit.ObligationID, "Pure layout; covered by the story's interaction test")
	if err != nil {
		t.Fatalf("Waive: %v", err)
	}
	if _, err := srv.Waive(ctx, sid, "not-an-id", "x"); !errors.Is(err, session.ErrUnknownObligation) {
		t.Errorf("unknown obligation = %v", err)
	}
	decisions, err := st.ListDecisionsBySession(ctx, sid)
	if err != nil || len(decisions) != 1 || decisions[0].ID != waived.DecisionID || decisions[0].Status != "rejected_alternative" {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	suite := obligationByRequirement(t, report.Obligations, "suite")
	attested, err := srv.Attest(ctx, sid, suite.ObligationID, "Test Files 12 passed (12)")
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if attested.Obligation.Status != session.ObligationSatisfied {
		t.Errorf("an attest-only requirement is met once attested: %+v", attested.Obligation)
	}

	// 5. Nothing required is open: the stop completes.
	res, err := srv.Stop(ctx, sid, "CheckoutPage done", "COMPLETED", true, false)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.Gate == nil || res.Gate.Mode != session.GateBlock || res.Gate.Outcome != session.GatePassed {
		t.Errorf("gate = %+v", res.Gate)
	}
	ops := queuedOps(t, st)
	if len(ops["POST_DECISION"]) != 1 || !strings.Contains(ops["POST_DECISION"][0], "Completeness waiver: Component test for CheckoutPage") {
		t.Errorf("waiver decision ops = %v", ops["POST_DECISION"])
	}
	if roles := ops["UPDATE_STATUS"]; len(roles) != 2 || !strings.Contains(roles[1], `"complete"`) {
		t.Errorf("status ops = %v", roles)
	}
	// The last checklist sync holds the definition of done as it stands:
	// every item resolved, each showing how.
	cls := ops[session.OpSyncChecklist]
	if len(cls) == 0 {
		t.Fatal("no checklist sync was queued")
	}
	var checklist session.ChecklistPayload
	if err := json.Unmarshal([]byte(cls[len(cls)-1]), &checklist); err != nil {
		t.Fatalf("checklist payload: %v", err)
	}
	var names []string
	for _, it := range checklist.Items {
		if !it.Resolved {
			t.Errorf("item %q is unresolved", it.Name)
		}
		names = append(names, it.Name)
	}
	slices.Sort(names)
	if want := []string{
		"[story] Storybook stories for CheckoutPage (src/features/payments/pages/CheckoutPage.tsx) — structural ✓",
		"[subject-file] New screen: CheckoutPage.tsx (src/features/payments/pages/CheckoutPage.tsx) — structural ✓",
		"[suite] Full web test suite passes (web) — attested",
		"[unit-test] Component test for CheckoutPage (src/features/payments/pages/CheckoutPage.tsx) — waived",
	}; checklist.ChecklistName != session.ChecklistName || !slices.Equal(names, want) {
		t.Errorf("checklist %q = %q, want %q", checklist.ChecklistName, names, want)
	}

	finished := ops["POST_COMMENT"][len(ops["POST_COMMENT"])-1]
	for _, want := range []string{
		"### Completeness (4/4)",
		"| Component test for CheckoutPage | `src/features/payments/pages/CheckoutPage.tsx` | required | waived | — | Pure layout; covered by the story's interaction test |",
		"| Full web test suite passes | `web` | required | satisfied | attest ✓ | Test Files 12 passed (12) |",
		"| New screen: CheckoutPage.tsx | `src/features/payments/pages/CheckoutPage.tsx` | required | satisfied | structural ✓ | — |",
	} {
		if !strings.Contains(finished, want) {
			t.Errorf("finished comment missing %q:\n%s", want, finished)
		}
	}
}

func TestCompleteness_EndToEnd_InheritOnRestart(t *testing.T) {
	dir, st, srv := newE2ERepo(t, "gate: report\n"+handWrittenManifest)
	ctx := context.Background()

	first, err := srv.Start(ctx, "task-rework", dir, true)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	writeFile(t, dir, "src/features/refunds/pages/RefundPage.tsx", "export function RefundPage() {}")
	if _, err := srv.Checkpoint(ctx, first.Session.ID, "refund page", nil, true); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "refund page")
	if _, err := srv.Stop(ctx, first.Session.ID, "", "COMPLETED", true, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Rework starts from the commit with the page in it, so the page isn't
	// in the new session's diff; what it still owed is inherited.
	second, err := srv.Start(ctx, "task-rework", dir, true)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	cc := second.AgenticContract.Completeness
	if got := describes(cc.Inherited); !slices.Equal(got, []string{"Component test for RefundPage", "Full web test suite passes", "Storybook stories for RefundPage"}) {
		t.Fatalf("inherited = %v", got)
	}
	for _, v := range cc.Inherited {
		if v.FromSession != first.Session.ID {
			t.Errorf("inherited %s from %q", v.Describe, v.FromSession)
		}
	}

	// The story lands: the inherited obligation is checked by its subject,
	// though RefundPage.tsx itself didn't change.
	writeFile(t, dir, "src/features/refunds/pages/RefundPage.stories.tsx", "export default { component: RefundPage }")
	cp, err := srv.Checkpoint(ctx, second.Session.ID, "refund story", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := cp.Completeness.NewlySatisfied; !slices.Equal(got, []string{"Storybook stories for RefundPage"}) {
		t.Errorf("newly_satisfied = %v", got)
	}
	obligations, err := st.ListObligations(ctx, second.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obligations {
		if o.InheritedFrom == "" {
			t.Errorf("unexpected new obligation %s/%s for %s", o.RuleID, o.RequirementID, o.Subject)
		}
	}
}
