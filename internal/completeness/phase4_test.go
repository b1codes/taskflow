package completeness

import (
	"maps"
	"slices"
	"testing"
)

// deletedManifest asks for a deleted screen's stories and test to go too.
const deletedManifest = `
version: 1
stacks:
  - { id: web, root: frontend }
rules:
  - id: react-screen-deleted
    stack: web
    describe: "Deleted screen"
    when:
      deleted: "src/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx"]
    require:
      - id: stories-removed
        describe: "Remove the stories for {name}"
        verify: { absent: "src/pages/{name}.stories.tsx" }
`

func TestEvaluate_DeletedTrigger(t *testing.T) {
	gone := []Change{{Path: "frontend/src/pages/Old.tsx", Type: Deleted}}

	t.Run("open while the stories remain", func(t *testing.T) {
		files := map[string]string{"frontend/src/pages/Old.stories.tsx": "export default {}"}
		res := evalFixture(t, deletedManifest, files, gone)
		if len(res.Findings) != 1 {
			t.Fatalf("findings = %+v", res.Findings)
		}
		f := res.Findings[0]
		if f.Subject != "frontend/src/pages/Old.tsx" || f.Satisfied || f.Describe != "Remove the stories for Old" {
			t.Errorf("finding = %+v", f)
		}
		if !slices.Equal(f.Expected, []string{AbsentPrefix + "frontend/src/pages/Old.stories.tsx"}) {
			t.Errorf("expected = %v", f.Expected)
		}
	})

	t.Run("satisfied once they're gone", func(t *testing.T) {
		res := evalFixture(t, deletedManifest, map[string]string{"frontend/src/pages/.keep": ""}, gone)
		if len(res.Findings) != 1 || !res.Findings[0].Satisfied || !slices.Equal(res.Findings[0].Levels, []string{"structural"}) {
			t.Errorf("findings = %+v", res.Findings)
		}
	})

	t.Run("a rename out of the stack root deletes the file inside it", func(t *testing.T) {
		res := evalFixture(t, deletedManifest, map[string]string{"frontend/.keep": ""},
			[]Change{{Path: "archive/Old.tsx", OldPath: "frontend/src/pages/Old.tsx", Type: Renamed}})
		if len(res.Findings) != 1 || res.Findings[0].Subject != "frontend/src/pages/Old.tsx" {
			t.Errorf("findings = %+v", res.Findings)
		}
	})

	t.Run("a rename into the stack root deletes nothing inside it", func(t *testing.T) {
		res := evalFixture(t, deletedManifest, map[string]string{"frontend/src/pages/Old.tsx": ""},
			[]Change{{Path: "frontend/src/pages/Old.tsx", OldPath: "legacy/src/pages/Old.tsx", Type: Renamed}})
		if len(res.Findings) != 0 {
			t.Errorf("findings = %+v", res.Findings)
		}
	})

	t.Run("excluded deletions don't fire", func(t *testing.T) {
		res := evalFixture(t, deletedManifest, map[string]string{"frontend/.keep": ""},
			[]Change{{Path: "frontend/src/pages/Old.stories.tsx", Type: Deleted}})
		if len(res.Findings) != 0 {
			t.Errorf("findings = %+v", res.Findings)
		}
	})
}

func TestEvaluate_DeletedRecheck(t *testing.T) {
	m, err := Parse([]byte(deletedManifest))
	if err != nil {
		t.Fatal(err)
	}
	rc := []Recheck{{RuleID: "react-screen-deleted", Subject: "frontend/src/pages/Old.tsx"}}

	t.Run("still applies while the file stays gone", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"frontend/src/pages/Old.stories.tsx": ""})
		res := Evaluate(root, m, nil, nil, rc)
		if len(res.Findings) != 1 || len(res.Dropped) != 0 {
			t.Errorf("findings = %+v, dropped = %+v", res.Findings, res.Dropped)
		}
	})

	t.Run("not found once the file is restored", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"frontend/src/pages/Old.tsx": ""})
		res := Evaluate(root, m, nil, nil, rc)
		if len(res.Findings) != 0 || len(res.Dropped) != 0 {
			t.Errorf("findings = %+v, dropped = %+v", res.Findings, res.Dropped)
		}
	})
}

func TestParse_AbsentValidated(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
rules:
  - id: r
    when: { deleted: "{name}.ts" }
    require: [{ id: q, describe: "x", verify: { absent: "{other}.ts" } }]
`))
	if err == nil {
		t.Fatal("an unknown variable in absent should be rejected")
	}
}

// commandManifest has a stack command check on a file rule and a
// stack-scoped suite that falls back to attestation without commands.
const commandManifest = `
version: 1
stacks:
  - id: web
    root: frontend
    env: { CI: "true" }
    commands:
      test: [{ if: { exists: "package.json" }, run: "npm test --" }]
      storybook-test: [{ if: { exists: ".storybook" }, run: "npm run test-storybook" }]
rules:
  - id: react-screen
    stack: web
    describe: "New screen"
    when: { added: "src/pages/{name}.tsx" }
    require:
      - id: visual-regression
        severity: recommended
        describe: "Visual regression for {name}"
        verify: { contains_in: { file: "src/pages/{name}.stories.tsx", any: ["play:"] } }
        run: { command: storybook-test }
      - id: contract
        describe: "Contract tests for {name}"
        run: { command: test, args: ["tests/contract", "{name} spec"] }
  - id: regression-suite
    stack: web
    describe: "Existing {stack} test suite still passes"
    when: { changed: "**" }
    scope: stack
    require:
      - id: suite
        describe: "Full {stack} test suite passes"
        run: { command: test }
        attest: "Run {command} in {root}"
        attest_when: no_command
`

func TestEvaluate_CommandChecks(t *testing.T) {
	files := map[string]string{
		"frontend/package.json":                   "{}",
		"frontend/.storybook/main.ts":             "",
		"frontend/src/pages/Checkout.tsx":         "",
		"frontend/src/pages/Checkout.stories.tsx": "export const A = { play: async () => {} }",
	}
	added := []Change{{Path: "frontend/src/pages/Checkout.tsx", Type: Added}}
	eval := func(t *testing.T, opts Options) map[string]Finding {
		t.Helper()
		root := t.TempDir()
		writeFiles(t, root, files)
		m, err := Parse([]byte(commandManifest))
		if err != nil {
			t.Fatal(err)
		}
		return findingsByKey(EvaluateWith(root, m, nil, added, nil, opts))
	}

	t.Run("off: no specs, attestation stands in", func(t *testing.T) {
		got := eval(t, Options{})
		for k, f := range got {
			if f.Command != nil {
				t.Errorf("%s has a command with commands off: %+v", k, f.Command)
			}
		}
		if suite := got["regression-suite/suite/web"]; suite.AttestPrompt != "Run npm test -- in frontend" {
			t.Errorf("suite = %+v", suite)
		}
	})

	t.Run("on: specs per requirement, no attestation for no_command", func(t *testing.T) {
		got := eval(t, Options{Commands: true})
		env := map[string]string{"CI": "true"}
		for k, want := range map[string]string{
			"react-screen/visual-regression/frontend/src/pages/Checkout.tsx": "npm run test-storybook",
			"react-screen/contract/frontend/src/pages/Checkout.tsx":          "npm test -- tests/contract 'Checkout spec'",
			"regression-suite/suite/web":                                     "npm test --",
		} {
			f := got[k]
			if f.Command == nil || f.Command.Line != want || f.Command.Root != "frontend" || !maps.Equal(f.Command.Env, env) {
				t.Errorf("%s command = %+v, want %q in frontend", k, f.Command, want)
			}
		}
		suite := got["regression-suite/suite/web"]
		if suite.AttestPrompt != "" || !suite.Satisfied || !suite.ChecksPassed {
			t.Errorf("suite with a command should need no attestation (the caller runs the command): %+v", suite)
		}
		// Structural results leave the command out.
		if vr := got["react-screen/visual-regression/frontend/src/pages/Checkout.tsx"]; !vr.Satisfied {
			t.Errorf("visual-regression's file check passed: %+v", vr)
		}
	})

	t.Run("on but unresolved: attestation fallback", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"frontend/src/pages/Checkout.tsx": ""})
		m, err := Parse([]byte(commandManifest))
		if err != nil {
			t.Fatal(err)
		}
		got := findingsByKey(EvaluateWith(root, m, nil, added, nil, Options{Commands: true}))
		suite := got["regression-suite/suite/web"]
		if suite.Command != nil || suite.AttestPrompt == "" {
			t.Errorf("an unresolved command should fall back to attestation: %+v", suite)
		}
	})
}
