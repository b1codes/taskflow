package completeness

import (
	"slices"
	"strings"
	"testing"
)

// reactManifest is the React-shaped manifest from spec §3, trimmed to what
// Phase 1 evaluates.
const reactManifest = `
version: 1
stacks:
  - id: web
    root: "."
    vars:
      pm:
        - { if: { exists: "pnpm-lock.yaml" }, value: "pnpm" }
        - { value: "npm" }
    commands:
      test:
        - { if: { has_script: "test" }, run: "{pm} test" }
rules:
  - id: react-screen
    source: framework/react-web
    stack: web
    describe: "New screen"
    when:
      added: "src/features/{feature}/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx", "**/*.test.tsx"]
    plannable: true
    require:
      - id: story
        source: testing/storybook
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.stories.tsx"
          contains: ["export default", "{name}"]
      - id: unit-test
        describe: "Component test for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.test.tsx"
      - id: e2e
        describe: "E2E coverage for the {name} route"
        verify:
          changed: "e2e/**/*.spec.ts"
          contains_any: ["{name}", "{feature}"]
        attest: "Name the spec file and test cases that cover {name}"
      - id: visual-regression
        severity: recommended
        describe: "Visual/interaction regression for {name} via its stories"
        verify:
          contains_in: { file: "src/features/{feature}/pages/{name}.stories.tsx", any: ["play:", "parameters:"] }
      - id: contract
        describe: "Contract tests for changed {feature} services"
        only_if:
          changed: "src/features/{feature}/services/*.ts"
        verify:
          changed: "tests/contract/**"
  - id: regression-suite
    stack: web
    describe: "Existing {stack} test suite still passes"
    when: { changed: "**" }
    scope: stack
    require:
      - id: suite
        describe: "Full {stack} test suite passes"
        run: { command: test }
        attest: "Run ` + "`{command}`" + ` in ` + "`{root}`" + ` and paste the final summary line"
        attest_when: no_command
`

func evalFixture(t *testing.T, manifest string, files map[string]string, changes []Change) *Result {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	m, err := Parse([]byte(manifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o, err := LoadOverrides(root)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	return Evaluate(root, m, o, changes)
}

func findingsByKey(res *Result) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range res.Findings {
		out[f.RuleID+"/"+f.RequirementID+"/"+f.Subject] = f
	}
	return out
}

const screen = "src/features/payments/pages/CheckoutPage.tsx"

func TestEvaluate_ReactScreen(t *testing.T) {
	added := []Change{{Path: screen, Type: Added}}

	t.Run("new screen with nothing else opens its requirements", func(t *testing.T) {
		res := evalFixture(t, reactManifest, map[string]string{screen: "export const CheckoutPage = 1"}, added)
		got := findingsByKey(res)
		for _, req := range []string{"story", "unit-test", "e2e", "visual-regression"} {
			f, ok := got["react-screen/"+req+"/"+screen]
			if !ok {
				t.Fatalf("missing finding for %s; got %v", req, got)
			}
			if f.Satisfied {
				t.Errorf("%s: expected open", req)
			}
		}
		if _, ok := got["react-screen/contract/"+screen]; ok {
			t.Errorf("contract should not apply: no services changed")
		}
		story := got["react-screen/story/"+screen]
		if story.Describe != "Storybook stories for CheckoutPage" || story.Source != "testing/storybook" {
			t.Errorf("story finding = %+v", story)
		}
		if !slices.Equal(story.Expected, []string{"src/features/payments/pages/CheckoutPage.stories.tsx"}) {
			t.Errorf("story expected = %v", story.Expected)
		}
		if got["react-screen/unit-test/"+screen].Source != "framework/react-web" {
			t.Errorf("requirement without source should inherit the rule's")
		}
		if got["react-screen/visual-regression/"+screen].Severity != SeverityRecommended {
			t.Errorf("visual-regression severity not kept")
		}
		suite, ok := got["regression-suite/suite/web"]
		if !ok || suite.Satisfied {
			t.Fatalf("expected an open stack-scoped regression obligation, got %+v", suite)
		}
		if suite.AttestPrompt != "Run `the project's test suite` in `.` and paste the final summary line" {
			t.Errorf("attest prompt = %q", suite.AttestPrompt)
		}
	})

	t.Run("adding the artifacts satisfies them", func(t *testing.T) {
		files := map[string]string{
			screen: "x",
			"src/features/payments/pages/CheckoutPage.stories.tsx": "export default { component: CheckoutPage }\nexport const Primary = { play: async () => {} }",
			"src/features/payments/pages/CheckoutPage.test.tsx":    "test()",
			"e2e/checkout.spec.ts":                                 "test('CheckoutPage loads')",
			"package.json":                                         `{"scripts": {"test": "vitest"}}`,
			"pnpm-lock.yaml":                                       "",
		}
		changes := append(added,
			Change{Path: "src/features/payments/pages/CheckoutPage.stories.tsx", Type: Added},
			Change{Path: "src/features/payments/pages/CheckoutPage.test.tsx", Type: Added},
			Change{Path: "e2e/checkout.spec.ts", Type: Modified},
		)
		got := findingsByKey(evalFixture(t, reactManifest, files, changes))
		for _, req := range []string{"story", "unit-test", "visual-regression"} {
			f := got["react-screen/"+req+"/"+screen]
			if !f.Satisfied || !slices.Equal(f.Levels, []string{"structural"}) {
				t.Errorf("%s: expected satisfied structurally, got %+v", req, f)
			}
		}
		e2e := got["react-screen/e2e/"+screen]
		if e2e.Satisfied || !slices.Equal(e2e.Levels, []string{"structural"}) {
			t.Errorf("e2e passes its file check but still needs an attestation: %+v", e2e)
		}
		if e2e.AttestPrompt != "Name the spec file and test cases that cover CheckoutPage" {
			t.Errorf("e2e attest prompt = %q", e2e.AttestPrompt)
		}
		if p := got["regression-suite/suite/web"].AttestPrompt; !strings.Contains(p, "`pnpm test`") {
			t.Errorf("regression prompt should name the resolved command, got %q", p)
		}
		if len(got) != 5 {
			t.Errorf("story/test files must not trigger the screen rule themselves; got %d findings", len(got))
		}
	})

	t.Run("changed services bring in the contract requirement", func(t *testing.T) {
		changes := append(added, Change{Path: "src/features/payments/services/api.ts", Type: Modified})
		got := findingsByKey(evalFixture(t, reactManifest, map[string]string{screen: "x"}, changes))
		f, ok := got["react-screen/contract/"+screen]
		if !ok || f.Satisfied {
			t.Fatalf("expected open contract obligation, got %+v (ok=%v)", f, ok)
		}
		if f.Describe != "Contract tests for changed payments services" {
			t.Errorf("describe = %q", f.Describe)
		}
	})
}

func TestEvaluate_TriggerKinds(t *testing.T) {
	manifest := `
version: 1
rules:
  - id: added
    when: { added: "src/{name}.ts" }
    require: [{ id: q, describe: "{name}", verify: { exists: "nope" } }]
  - id: modified
    when: { modified: "src/{name}.ts" }
    require: [{ id: q, describe: "{name}", verify: { exists: "nope" } }]
  - id: changed
    when: { changed: "src/{name}.ts" }
    require: [{ id: q, describe: "{name}", verify: { exists: "nope" } }]
  - id: deleted
    when: { deleted: "src/{name}.ts" }
    require: [{ id: q, describe: "{name}", verify: { exists: "nope" } }]
`
	changes := []Change{
		{Path: "src/new.ts", Type: Added},
		{Path: "src/edited.ts", Type: Modified},
		{Path: "src/moved.ts", OldPath: "src/old.ts", Type: Renamed},
		{Path: "src/gone.ts", Type: Deleted},
	}
	got := map[string][]string{}
	for _, f := range evalFixture(t, manifest, nil, changes).Findings {
		got[f.RuleID] = append(got[f.RuleID], f.Subject)
	}
	want := map[string][]string{
		"added":    {"src/moved.ts", "src/new.ts"},
		"modified": {"src/edited.ts"},
		"changed":  {"src/edited.ts", "src/moved.ts", "src/new.ts"},
	}
	for rule, subjects := range want {
		if !slices.Equal(got[rule], subjects) {
			t.Errorf("%s fired for %v, want %v", rule, got[rule], subjects)
		}
	}
	if len(got["deleted"]) != 0 {
		t.Errorf("deleted triggers must not act yet, fired for %v", got["deleted"])
	}
}

func TestEvaluate_VerifyPrimitives(t *testing.T) {
	files := map[string]string{
		"src/Foo.ts":        "export const Foo = 1; export const Bar = 2",
		"src/Foo.test.ts":   "describe('Foo')",
		"docs/notes.md":     "release notes for Foo",
		"docs/other.md":     "nothing here",
		"src/Foo.README.md": "play: true",
	}
	changes := []Change{
		{Path: "src/Foo.ts", Type: Added},
		{Path: "docs/notes.md", Type: Modified},
		{Path: "docs/deleted.md", Type: Deleted},
	}
	tests := []struct {
		name   string
		verify string
		want   bool
	}{
		{"exists", `{ exists: "src/{name}.test.ts" }`, true},
		{"exists missing", `{ exists: "src/{name}.spec.ts" }`, false},
		{"exists glob", `{ exists: "src/*.test.ts" }`, true},
		{"changed", `{ changed: "docs/*.md" }`, true},
		{"changed ignores deletions", `{ changed: "docs/deleted.md" }`, false},
		{"changed unchanged file", `{ changed: "docs/other.md" }`, false},
		{"contains on exists target", `{ exists: "src/{name}.test.ts", contains: ["describe", "{name}"] }`, true},
		{"contains needs all", `{ exists: "src/{name}.test.ts", contains: ["describe", "it("] }`, false},
		{"contains on subject", `{ contains: ["export const {name}"] }`, true},
		{"contains_any on changed", `{ changed: "docs/**", contains_any: ["nope", "{name}"] }`, true},
		{"contains_any none match", `{ changed: "docs/**", contains_any: ["nope"] }`, false},
		{"exists and changed intersect", `{ exists: "docs/*.md", changed: "docs/other.md" }`, false},
		{"contains_in any", `{ contains_in: { file: "src/{name}.README.md", any: ["play:", "parameters:"] } }`, true},
		{"contains_in all", `{ contains_in: { file: "src/{name}.README.md", all: ["play:", "parameters:"] } }`, false},
		{"contains_in missing file", `{ contains_in: { file: "src/none.md", any: ["x"] } }`, false},
		{"min_matches on subject", `{ min_matches: { pattern: "export const \\w+", count: 2 } }`, true},
		{"min_matches too few", `{ min_matches: { pattern: "export const \\w+", count: 3 } }`, false},
		{"min_matches in file", `{ min_matches: { pattern: "Foo", count: 1, file: "docs/notes.md" } }`, true},
		{"any_of", `{ any_of: [{ exists: "nope" }, { exists: "src/{name}.test.ts" }] }`, true},
		{"any_of none", `{ any_of: [{ exists: "nope" }, { exists: "nada" }] }`, false},
		{"all_of", `{ all_of: [{ exists: "src/{name}.test.ts" }, { changed: "docs/*.md" }] }`, true},
		{"all_of one fails", `{ all_of: [{ exists: "src/{name}.test.ts" }, { exists: "nope" }] }`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := `
version: 1
rules:
  - id: r
    when: { added: "src/{name}.ts" }
    require:
      - id: q
        describe: "d"
        verify: ` + tt.verify + "\n"
			res := evalFixture(t, manifest, files, changes)
			if len(res.Findings) != 1 {
				t.Fatalf("expected one finding, got %+v", res.Findings)
			}
			if got := res.Findings[0].Satisfied; got != tt.want {
				t.Errorf("satisfied = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_Templates(t *testing.T) {
	manifest := `
version: 1
stacks: [{ id: web, root: frontend, commands: { test: [{ run: "npm test" }] } }]
rules:
  - id: r
    stack: web
    when: { added: "src/{feature}/{name}.tsx" }
    require:
      - id: q
        describe: "{name} in {feature} · {stem} · {dir} · {path} · {stack} · {root} · {command}"
        run: { command: test, args: ["src/{feature}", "it's"] }
        attest: "run {command}"
`
	res := evalFixture(t, manifest, map[string]string{"frontend/src/pay/Checkout.tsx": ""},
		[]Change{{Path: "frontend/src/pay/Checkout.tsx", Type: Added}})
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v", res.Findings)
	}
	f := res.Findings[0]
	want := `Checkout in pay · Checkout · src/pay · src/pay/Checkout.tsx · web · frontend · npm test src/pay 'it'\''s'`
	if f.Describe != want {
		t.Errorf("describe = %q\nwant       %q", f.Describe, want)
	}
	if f.AttestPrompt != `run npm test src/pay 'it'\''s'` {
		t.Errorf("attest prompt = %q (args rendered and shell-quoted)", f.AttestPrompt)
	}
	if f.Subject != "frontend/src/pay/Checkout.tsx" {
		t.Errorf("subject must be project-relative, got %q", f.Subject)
	}
}

func TestEvaluate_Monorepo(t *testing.T) {
	manifest := `
version: 1
stacks:
  - { id: web, root: frontend, commands: { test: [{ if: { exists: "package.json" }, run: "npm test" }] } }
  - { id: api, root: backend, commands: { test: [{ if: { exists: "uv.lock" }, run: "uv run pytest" }] } }
rules:
  - id: regression-suite
    stack: web
    describe: "Existing {stack} test suite still passes"
    when: { changed: "**" }
    scope: stack
    require: [{ id: suite, describe: "Full {stack} suite", attest: "Run {command}", attest_when: no_command }]
  - id: regression-suite-api
    stack: api
    describe: "Existing {stack} test suite still passes"
    when: { changed: "**" }
    scope: stack
    require: [{ id: suite, describe: "Full {stack} suite", attest: "Run {command}", attest_when: no_command }]
`
	files := map[string]string{"frontend/package.json": "{}", "backend/uv.lock": "", "backend/app/main.py": ""}
	res := evalFixture(t, manifest, files, []Change{
		{Path: "backend/app/main.py", Type: Modified},
		{Path: "backend/app/models.py", Type: Added},
		{Path: "README.md", Type: Modified},
	})
	if len(res.Findings) != 1 {
		t.Fatalf("a backend-only diff should create only the api regression obligation, got %+v", res.Findings)
	}
	f := res.Findings[0]
	if f.Subject != "api" || f.AttestPrompt != "Run uv run pytest" || f.Satisfied {
		t.Errorf("finding = %+v", f)
	}
}

func TestEvaluate_MissingStackRootSkipsRules(t *testing.T) {
	manifest := `
version: 1
stacks: [{ id: web, root: frontend }]
rules:
  - id: r
    stack: web
    when: { added: "{name}.ts" }
    require: [{ id: q, describe: "d", verify: { exists: "x" } }]
`
	res := evalFixture(t, manifest, nil, []Change{{Path: "frontend/a.ts", Type: Added}})
	if len(res.Findings) != 0 {
		t.Errorf("expected no findings, got %+v", res.Findings)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "stack web skipped") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestEvaluate_RuleSummaries(t *testing.T) {
	res := evalFixture(t, reactManifest, nil, nil)
	if len(res.Rules) != 2 {
		t.Fatalf("rules = %+v", res.Rules)
	}
	want := []string{"story (required)", "unit-test (required)", "e2e (required)", "visual-regression (recommended)", "contract (required, conditional)"}
	if r := res.Rules[0]; r.ID != "react-screen" || !r.Plannable || !slices.Equal(r.Requires, want) {
		t.Errorf("summary = %+v", r)
	}
}
