package completeness

import (
	"slices"
	"strings"
	"testing"
)

const planManifest = `
version: 1
stacks: [{ id: web, root: web }]
rules:
  - id: react-screen
    stack: web
    describe: "New {feature} screen"
    when:
      added: "src/features/{feature}/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx"]
    plannable: true
    plan_hints: ["screen", "page"]
    require:
      - id: story
        describe: "Storybook stories for {name}"
        verify: { exists: "src/features/{feature}/pages/{name}.stories.tsx" }
      - id: e2e
        describe: "E2E coverage for {name}"
        verify: { changed: "e2e/**/*.spec.ts" }
        attest: "Name the spec covering {name}"
  - id: any-module
    when: { added: "lib/**/*.go" }
    plannable: true
    require: [{ id: test, describe: "d", verify: { exists: "{dir}/{stem}_test.go" } }]
  - id: suite
    stack: web
    scope: stack
    when: { changed: "**" }
    plannable: true
    require: [{ id: q, describe: "suite", attest: "ran it" }]
  - id: unplannable
    when: { added: "docs/{name}.md" }
    require: [{ id: q, describe: "d", verify: { exists: "docs/{name}.md" } }]
`

func TestPlanSubject(t *testing.T) {
	m, err := Parse([]byte(planManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ps, err := PlanSubject(m, "react-screen", map[string]string{"feature": "payments", "name": "CheckoutPage"})
	if err != nil || ps.Subject != "web/src/features/payments/pages/CheckoutPage.tsx" || ps.RuleID != "react-screen" {
		t.Fatalf("PlanSubject = %+v, %v; want the subject under the stack root", ps, err)
	}
	if ps, err := PlanSubject(m, "suite", nil); err != nil || ps.Subject != "web" {
		t.Errorf("stack-scoped plan = %+v, %v; want subject web", ps, err)
	}

	tests := []struct {
		name   string
		rule   string
		vars   map[string]string
		reason string
	}{
		{"unknown rule", "nope", nil, `unknown rule "nope" (plannable rules: react-screen, any-module, suite)`},
		{"not plannable", "unplannable", map[string]string{"name": "x"}, `rule "unplannable" is not plannable`},
		{"missing var", "react-screen", map[string]string{"feature": "payments"}, "missing name"},
		{"unknown var", "react-screen", map[string]string{"feature": "p", "name": "N", "page": "x"}, "not page"},
		{"slash in var", "react-screen", map[string]string{"feature": "../..", "name": "N"}, "without slashes"},
		{"dot-dot var", "react-screen", map[string]string{"feature": "..", "name": "N"}, "without slashes"},
		{"excluded subject", "react-screen", map[string]string{"feature": "p", "name": "N.stories"}, "excluded"},
		{"wildcard trigger", "any-module", nil, "has wildcards"},
		{"stack rule with vars", "suite", map[string]string{"x": "y"}, "takes no vars"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanSubject(m, tt.rule, tt.vars)
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("error = %v, want one mentioning %q", err, tt.reason)
			}
		})
	}
}

func TestEvaluate_DeclaredRecheck(t *testing.T) {
	m, err := Parse([]byte(planManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"web/package.json": "{}"})
	planned := "web/src/features/payments/pages/CheckoutPage.tsx"

	// Nothing exists yet: a declared subject is checked anyway.
	res := Evaluate(root, m, &Overrides{}, nil, []Recheck{{RuleID: "react-screen", Subject: planned, Declared: true, SubjectFile: true}})
	got := findingsByKey(res)
	file, ok := got["react-screen/subject-file/"+planned]
	if !ok || file.Satisfied || file.Describe != "New payments screen: CheckoutPage.tsx" || !slices.Equal(file.Expected, []string{planned}) || file.Severity != SeverityRequired {
		t.Errorf("subject-file finding = %+v", file)
	}
	if f, ok := got["react-screen/story/"+planned]; !ok || f.Satisfied || !slices.Equal(f.Expected, []string{"web/src/features/payments/pages/CheckoutPage.stories.tsx"}) {
		t.Errorf("story finding = %+v", f)
	}
	if len(got) != 3 {
		t.Errorf("findings = %v, want subject-file, story, e2e", got)
	}

	// An undeclared recheck of a missing file is still dropped, and a
	// declared one without SubjectFile gets no subject-file finding.
	res = Evaluate(root, m, &Overrides{}, nil, []Recheck{{RuleID: "react-screen", Subject: planned}})
	if len(res.Findings) != 0 {
		t.Errorf("undeclared recheck of a missing file = %+v", res.Findings)
	}
	res = Evaluate(root, m, &Overrides{}, nil, []Recheck{{RuleID: "react-screen", Subject: planned, Declared: true}})
	if _, ok := findingsByKey(res)["react-screen/subject-file/"+planned]; ok || len(res.Findings) != 2 {
		t.Errorf("declared recheck without subject file = %+v", res.Findings)
	}

	// The files land: the subject file and story are satisfied. The e2e
	// requirement's change is in, but it still needs an attestation.
	writeFiles(t, root, map[string]string{
		planned: "export default function CheckoutPage() {}",
		"web/src/features/payments/pages/CheckoutPage.stories.tsx": "export default {}",
		"web/e2e/checkout.spec.ts":                                 "test('CheckoutPage')",
	})
	changes := []Change{{Path: planned, Type: Added}, {Path: "web/e2e/checkout.spec.ts", Type: Added}}
	res = Evaluate(root, m, &Overrides{}, changes, []Recheck{{RuleID: "react-screen", Subject: planned, Declared: true, SubjectFile: true}})
	got = findingsByKey(res)
	if f := got["react-screen/subject-file/"+planned]; !f.Satisfied || !f.ChecksPassed || !slices.Equal(f.Levels, []string{"structural"}) {
		t.Errorf("subject-file once the file exists = %+v", f)
	}
	if f := got["react-screen/e2e/"+planned]; f.Satisfied || !f.ChecksPassed || f.AttestPrompt != "Name the spec covering CheckoutPage" {
		t.Errorf("e2e = %+v: checks pass, but it must stay unsatisfied until attested", f)
	}
	if n := len(res.Findings); n != 4 {
		t.Errorf("findings = %d, want the diff's three (counted once) plus the subject file", n)
	}
}

func TestEvaluate_AttestWhen(t *testing.T) {
	// Commands aren't executed, so attest_when: no_command needs an
	// attestation even when the stack's command resolves.
	res := evalFixture(t, reactManifest, map[string]string{"package.json": `{"scripts": {"test": "vitest"}}`}, []Change{{Path: "src/a.ts", Type: Added}})
	f, ok := findingsByKey(res)["regression-suite/suite/web"]
	if !ok || f.Satisfied || !f.ChecksPassed || f.AttestPrompt != "Run `npm test` in `.` and paste the final summary line" {
		t.Errorf("regression suite = %+v", f)
	}
}

func TestEvaluate_RuleSummaryPlanFields(t *testing.T) {
	res := evalFixture(t, planManifest, map[string]string{"web/x": ""}, nil)
	for _, r := range res.Rules {
		switch r.ID {
		case "react-screen":
			if !slices.Equal(r.PlanHints, []string{"screen", "page"}) || !slices.Equal(r.PlanVars, []string{"feature", "name"}) {
				t.Errorf("react-screen summary = %+v", r)
			}
		case "unplannable":
			if r.PlanVars != nil {
				t.Errorf("a rule that can't be planned has no plan vars: %+v", r)
			}
		}
	}
}
