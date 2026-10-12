package adapters

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func run(t *testing.T, dir string, spec session.CommandSpec, timeout time.Duration) session.CommandCheck {
	t.Helper()
	e := NewCompletenessEvaluator().WithCommands(true, timeout)
	out := e.RunCommands(context.Background(), &session.Session{ProjectPath: dir}, []session.CommandSpec{spec})
	if len(out) != 1 {
		t.Fatalf("RunCommands returned %d results", len(out))
	}
	return out[0]
}

func TestRunCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("exit 0 passes, in the stack root, with its env over the inherited one", func(t *testing.T) {
		t.Parallel()
		c := run(t, dir, session.CommandSpec{Line: `test "$CI" = true && test -n "$HOME" && basename "$(pwd -P)"`, Root: "frontend",
			Env: map[string]string{"CI": "true"}}, time.Minute)
		if !c.Passed || c.ExitCode != 0 || strings.TrimSpace(c.Output) != "frontend" || c.Command == "" || c.Root != "frontend" {
			t.Errorf("check = %+v", c)
		}
	})

	t.Run("a non-zero exit fails with its code", func(t *testing.T) {
		t.Parallel()
		c := run(t, dir, session.CommandSpec{Line: "echo boom >&2; exit 3", Root: "."}, time.Minute)
		if c.Passed || c.ExitCode != 3 || c.Error != "" || strings.TrimSpace(c.Output) != "boom" {
			t.Errorf("check = %+v", c)
		}
	})

	t.Run("stdin is closed, so nothing waits on it", func(t *testing.T) {
		t.Parallel()
		c := run(t, dir, session.CommandSpec{Line: "cat", Root: "."}, 5*time.Second)
		if !c.Passed {
			t.Errorf("check = %+v", c)
		}
	})

	t.Run("a timeout kills everything the command started", func(t *testing.T) {
		t.Parallel()
		began := time.Now()
		c := run(t, dir, session.CommandSpec{Line: "echo started; sleep 30 & sleep 30", Root: "."}, 300*time.Millisecond)
		if elapsed := time.Since(began); elapsed > 3*time.Second {
			t.Errorf("took %s: the background sleep outlived the timeout", elapsed)
		}
		if c.Passed || !c.TimedOut || c.ExitCode != -1 || !strings.Contains(c.Error, "timed out after 300ms") || !strings.Contains(c.Output, "started") {
			t.Errorf("check = %+v", c)
		}
	})

	t.Run("a missing root fails without running", func(t *testing.T) {
		t.Parallel()
		c := run(t, dir, session.CommandSpec{Line: "true", Root: "nope"}, time.Minute)
		if c.Passed || c.ExitCode != -1 || !strings.Contains(c.Error, `"nope"`) {
			t.Errorf("check = %+v", c)
		}
	})

	t.Run("output keeps its end", func(t *testing.T) {
		t.Parallel()
		c := run(t, dir, session.CommandSpec{Line: "head -c 20000 /dev/zero | tr '\\0' a; echo; echo 'Tests: 3 passed'", Root: "."}, time.Minute)
		if !c.Passed || len(c.Output) > maxCommandOutput+64 || !strings.HasPrefix(c.Output, "…(") || !strings.HasSuffix(c.Output, "Tests: 3 passed\n") {
			t.Errorf("output (%d bytes) = %.60q … %q", len(c.Output), c.Output, c.Output[max(0, len(c.Output)-40):])
		}
	})
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()
	b := &tailBuffer{max: 8}
	for _, s := range []string{"abc", "defgh", "ijklmnopqrstu", "é"} {
		_, _ = b.Write([]byte(s))
	}
	// The last 8 bytes split nothing; "é" is two bytes.
	if got := b.String(); got != "…(15 bytes cut)\npqrstué" {
		t.Errorf("String() = %q", got)
	}
	b = &tailBuffer{max: 3}
	_, _ = b.Write([]byte("xé")) // keeping 3 bytes would split nothing; 2 would
	b.max = 2
	if got := b.String(); got != "…(1 bytes cut)\né" {
		t.Errorf("String() = %q", got)
	}
}

// stacksManifest declares every stack in spec §3's defaults table, each in
// its own root, with the regression suite b1 pair emits per stack.
var stacksManifest = `
version: 1
stacks:
  - id: web
    source: framework/react-web
    root: web
    env: { CI: "true" }
    vars: &pm
      pm:
        - { if: { exists: "pnpm-lock.yaml" }, value: "pnpm" }
        - { if: { exists: "yarn.lock" }, value: "yarn" }
        - { if: { any: [{ exists: "bun.lock" }, { exists: "bun.lockb" }] }, value: "bun" }
        - { value: "npm" }
    commands: &jstest
      test:
        - { if: { has_script: "test:ci" }, run: "{pm} run test:ci" }
        - { if: { has_script: "test" }, run: "{pm} test" }
  - id: native
    source: framework/react-native
    root: mobile
    env: { CI: "true" }
    vars: *pm
    commands: *jstest
  - id: flutter
    source: framework/flutter
    root: app
    commands:
      test: [{ if: { exists: "pubspec.yaml" }, run: "flutter test" }]
  - id: ios
    source: framework/swiftui
    root: ios
    commands:
      test:
        - { if: { exists: "Package.swift" }, run: "swift test" }
        - { if: { exists: "*.xcodeproj" }, run: "xcodebuild test -scheme {scheme} -destination '{destination}'" }
  - id: django
    source: framework/django
    root: site
    vars: &py
      py:
        - { if: { exists: "uv.lock" }, value: "uv run" }
        - { if: { exists: "poetry.lock" }, value: "poetry run" }
        - { value: ".venv/bin/python -m" }
    commands:
      test:
        - if:
            any:
              - { exists: "pytest.ini" }
              - { exists: "conftest.py" }
              - { file_contains: { file: "pyproject.toml", text: "[tool.pytest" } }
          run: "{py} pytest"
        - { if: { exists: "manage.py" }, run: "{py} python manage.py test" }
  - id: api
    source: framework/fastapi
    root: backend
    vars: *py
    commands:
      test: [{ run: "{py} pytest" }]
rules:
` + suiteRule("web") + suiteRule("native") + suiteRule("flutter") + suiteRule("ios") + suiteRule("django") + suiteRule("api")

func suiteRule(stack string) string {
	return `
  - id: regression-suite-` + stack + `
    stack: ` + stack + `
    describe: "Existing {stack} test suite still passes"
    when: { changed: "**" }
    scope: stack
    require:
      - id: suite
        describe: "Full {stack} test suite passes"
        run: { command: test }
        attest: "Run {command} in {root} and paste the final summary line"
        attest_when: no_command
`
}

// fakeTool reports how it was run, where, and whether the stack env got
// through.
const fakeTool = "#!/bin/sh\necho \"ran: $(basename \"$0\") $* in $(basename \"$(pwd -P)\") CI=${CI:-unset}\"\n"

// Command resolution and execution for every stack in spec §3's defaults
// table, through the real evaluator: the change under one stack's root
// creates only that stack's regression obligation, whose command resolves
// from the project's files and runs in the stack root.
func TestCompletenessEvaluator_RunsStackCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := t.TempDir()
	for _, tool := range []string{"pnpm", "yarn", "bun", "npm", "flutter", "swift", "xcodebuild", "uv", "poetry"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(fakeTool), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cases := []struct {
		name    string
		files   map[string]string
		changed string
		want    string // the fake tool's report; "" = unresolved, attest instead
	}{
		{"react-web pnpm prefers test:ci", map[string]string{"web/pnpm-lock.yaml": "", "web/package.json": `{"scripts":{"test":"vitest","test:ci":"vitest run"}}`},
			"web/src/App.tsx", "ran: pnpm run test:ci in web CI=true"},
		{"react-web yarn", map[string]string{"web/yarn.lock": "", "web/package.json": `{"scripts":{"test":"jest"}}`},
			"web/src/App.tsx", "ran: yarn test in web CI=true"},
		{"react-web bun", map[string]string{"web/bun.lockb": "", "web/package.json": `{"scripts":{"test":"bun test"}}`},
			"web/src/App.tsx", "ran: bun test in web CI=true"},
		{"react-web npm fallback", map[string]string{"web/package.json": `{"scripts":{"test":"jest"}}`},
			"web/src/App.tsx", "ran: npm test in web CI=true"},
		{"react-native (Expo)", map[string]string{"mobile/package-lock.json": "", "mobile/package.json": `{"scripts":{"test":"jest-expo"}}`},
			"mobile/app/index.tsx", "ran: npm test in mobile CI=true"},
		{"flutter", map[string]string{"app/pubspec.yaml": "name: app"},
			"app/lib/main.dart", "ran: flutter test in app CI=unset"},
		{"swiftui SwiftPM", map[string]string{"ios/Package.swift": ""},
			"ios/Sources/App.swift", "ran: swift test in ios CI=unset"},
		{"swiftui Xcode with vars", map[string]string{"ios/App.xcodeproj/project.pbxproj": "",
			".agents/project/completeness.yaml": "stacks:\n  ios:\n    vars: { scheme: MyApp, destination: \"platform=iOS Simulator,name=iPhone 16\" }\n"},
			"ios/App/ContentView.swift", "ran: xcodebuild test -scheme MyApp -destination platform=iOS Simulator,name=iPhone 16 in ios CI=unset"},
		{"swiftui Xcode without vars", map[string]string{"ios/App.xcodeproj/project.pbxproj": ""},
			"ios/App/ContentView.swift", ""},
		{"django uv pytest", map[string]string{"site/uv.lock": "", "site/pytest.ini": "", "site/manage.py": ""},
			"site/blog/views.py", "ran: uv run pytest in site CI=unset"},
		{"django poetry manage.py", map[string]string{"site/poetry.lock": "", "site/manage.py": ""},
			"site/blog/views.py", "ran: poetry run python manage.py test in site CI=unset"},
		{"django-ninja venv pyproject pytest", map[string]string{"site/pyproject.toml": "[tool.pytest.ini_options]\n", "site/.venv/bin/python": fakeTool},
			"site/api/routes.py", "ran: python -m pytest in site CI=unset"},
		{"fastapi uv", map[string]string{"backend/uv.lock": ""},
			"backend/app/main.py", "ran: uv run pytest in backend CI=unset"},
		{"monorepo: a backend-only diff runs only the api suite", map[string]string{"web/package.json": `{"scripts":{"test":"jest"}}`, "backend/uv.lock": ""},
			"backend/app/main.py", "ran: uv run pytest in backend CI=unset"},
		{"project override pins the command", map[string]string{"backend/uv.lock": "",
			".agents/project/completeness.yaml": "stacks:\n  api:\n    commands: { test: \"uv run pytest -x\" }\n"},
			"backend/app/main.py", "ran: uv run pytest -x in backend CI=unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitRun := func(args ...string) {
				t.Helper()
				if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			write := func(rel, content string) {
				t.Helper()
				p := filepath.Join(dir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o644)
				if content == fakeTool {
					mode = 0o755
				}
				if err := os.WriteFile(p, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			gitRun("init", "-q", "-b", "main")
			gitRun("config", "user.name", "Test")
			gitRun("config", "user.email", "test@test.com")
			gitRun("config", "commit.gpgsign", "false")
			write(".agents/completeness.yaml", stacksManifest)
			for rel, content := range tc.files {
				write(rel, content)
			}
			gitRun("add", "-A")
			gitRun("commit", "-q", "-m", "fixture")
			base, err := NewCompletenessEvaluator().BaseSHA(dir)
			if err != nil {
				t.Fatal(err)
			}
			write(tc.changed, "changed")

			e := NewCompletenessEvaluator().WithCommands(true, time.Minute)
			sess := &session.Session{ID: "s", ProjectPath: dir, BaseSHA: base}
			ev, err := e.Evaluate(context.Background(), sess, nil)
			if err != nil || ev.Error != "" {
				t.Fatalf("Evaluate: %v %s", err, ev.Error)
			}
			if len(ev.Findings) != 1 {
				t.Fatalf("want one regression obligation for the changed stack, got %+v", ev.Findings)
			}
			f := ev.Findings[0]
			if tc.want == "" {
				if f.Command != nil || f.AttestPrompt == "" {
					t.Errorf("an unresolved command should fall back to attestation: %+v", f)
				}
				return
			}
			if f.Command == nil || f.AttestPrompt != "" {
				t.Fatalf("finding = %+v, want a command check and no attestation", f)
			}
			checks := e.RunCommands(context.Background(), sess, []session.CommandSpec{*f.Command})
			if c := checks[0]; !c.Passed || strings.TrimSpace(c.Output) != tc.want {
				t.Errorf("ran %q in %s: %+v\nwant output %q", f.Command.Line, f.Command.Root, c, tc.want)
			}
		})
	}
}
