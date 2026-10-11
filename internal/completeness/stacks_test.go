package completeness

import (
	"strings"
	"testing"
)

// defaultStacks mirrors the per-framework detection b1 modules ship (spec §3).
const defaultStacks = `
version: 1
stacks:
  - id: web
    root: "."
    env: { CI: "true" }
    vars:
      pm:
        - { if: { exists: "pnpm-lock.yaml" }, value: "pnpm" }
        - { if: { exists: "yarn.lock" }, value: "yarn" }
        - { if: { any: [{ exists: "bun.lock" }, { exists: "bun.lockb" }] }, value: "bun" }
        - { value: "npm" }
    commands:
      test:
        - { if: { has_script: "test:ci" }, run: "{pm} run test:ci" }
        - { if: { has_script: "test" }, run: "{pm} test" }
  - id: flutter
    root: "."
    commands:
      test:
        - { if: { exists: "pubspec.yaml" }, run: "flutter test" }
  - id: ios
    root: "."
    commands:
      test:
        - { if: { exists: "Package.swift" }, run: "swift test" }
        - { if: { exists: "*.xcodeproj" }, run: "xcodebuild test -scheme {scheme} -destination '{destination}'" }
  - id: api
    root: "."
    vars:
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
rules: []
`

func resolveFixture(t *testing.T, files map[string]string, stackID string) ResolvedStack {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	m, err := Parse([]byte(defaultStacks))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o, err := LoadOverrides(root)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	for _, rs := range ResolveStacks(root, m, o) {
		if rs.ID == stackID {
			return rs
		}
	}
	t.Fatalf("stack %s not resolved", stackID)
	return ResolvedStack{}
}

func TestResolveStacks_Commands(t *testing.T) {
	scripts := func(names ...string) string {
		var parts []string
		for _, n := range names {
			parts = append(parts, `"`+n+`": "x"`)
		}
		return `{"scripts": {` + strings.Join(parts, ",") + `}}`
	}
	tests := []struct {
		name  string
		stack string
		files map[string]string
		want  string
	}{
		{"pnpm lockfile", "web", map[string]string{"pnpm-lock.yaml": "", "package.json": scripts("test")}, "pnpm test"},
		{"yarn lockfile", "web", map[string]string{"yarn.lock": "", "package.json": scripts("test")}, "yarn test"},
		{"bun.lock", "web", map[string]string{"bun.lock": "", "package.json": scripts("test")}, "bun test"},
		{"bun.lockb", "web", map[string]string{"bun.lockb": "", "package.json": scripts("test")}, "bun test"},
		{"npm fallback", "web", map[string]string{"package.json": scripts("test")}, "npm test"},
		{"test:ci beats test", "web", map[string]string{"pnpm-lock.yaml": "", "package.json": scripts("test", "test:ci")}, "pnpm run test:ci"},
		{"flutter", "flutter", map[string]string{"pubspec.yaml": ""}, "flutter test"},
		{"swiftpm", "ios", map[string]string{"Package.swift": "", "App.xcodeproj/project.pbxproj": ""}, "swift test"},
		{"uv + pytest.ini", "api", map[string]string{"uv.lock": "", "pytest.ini": ""}, "uv run pytest"},
		{"poetry + conftest", "api", map[string]string{"poetry.lock": "", "conftest.py": ""}, "poetry run pytest"},
		{"venv + pyproject pytest", "api", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"}, ".venv/bin/python -m pytest"},
		{"django manage.py test", "api", map[string]string{"uv.lock": "", "manage.py": "", "pyproject.toml": "[project]\n"}, "uv run python manage.py test"},
		{"pinned override", "api", map[string]string{
			"uv.lock": "", "pytest.ini": "",
			OverridesPath: "stacks:\n  api:\n    commands: { test: \"uv run pytest -x\" }\nrules: { disable: [x] }\n",
		}, "uv run pytest -x"},
		{"xcode with override vars", "ios", map[string]string{
			"App.xcodeproj/project.pbxproj": "",
			OverridesPath:                   "stacks:\n  ios:\n    vars: { scheme: \"MyApp\", destination: \"platform=iOS Simulator,name=iPhone 16\" }\n",
		}, "xcodebuild test -scheme MyApp -destination 'platform=iOS Simulator,name=iPhone 16'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := resolveFixture(t, tt.files, tt.stack)
			if got := rs.Commands["test"]; got != tt.want {
				t.Errorf("test command = %q, want %q (warnings %v)", got, tt.want, rs.Warnings)
			}
			if len(rs.Warnings) != 0 {
				t.Errorf("unexpected warnings: %v", rs.Warnings)
			}
		})
	}
}

func TestResolveStacks_Unresolved(t *testing.T) {
	t.Run("xcode without vars names the vars to set", func(t *testing.T) {
		rs := resolveFixture(t, map[string]string{"App.xcodeproj/project.pbxproj": ""}, "ios")
		if rs.Commands["test"] != "" {
			t.Fatalf("expected no test command, got %q", rs.Commands["test"])
		}
		if len(rs.Warnings) != 1 ||
			!strings.Contains(rs.Warnings[0], "stacks.ios.vars.scheme and stacks.ios.vars.destination") ||
			!strings.Contains(rs.Warnings[0], OverridesPath) {
			t.Errorf("warnings = %v", rs.Warnings)
		}
	})
	t.Run("no candidate applies names the command to pin", func(t *testing.T) {
		rs := resolveFixture(t, map[string]string{"package.json": `{"scripts": {"build": "x"}}`}, "web")
		if _, ok := rs.Commands["test"]; !ok || rs.Commands["test"] != "" {
			t.Fatalf("expected test listed as unresolved, got %v", rs.Commands)
		}
		if len(rs.Warnings) != 1 || !strings.Contains(rs.Warnings[0], "stacks.web.commands.test") {
			t.Errorf("warnings = %v", rs.Warnings)
		}
	})
	t.Run("missing root skips the stack", func(t *testing.T) {
		root := t.TempDir()
		m, err := Parse([]byte("version: 1\nstacks:\n  - id: web\n    root: frontend\n"))
		if err != nil {
			t.Fatal(err)
		}
		rs := ResolveStacks(root, m, nil)[0]
		if !rs.Missing || len(rs.Warnings) != 1 || !strings.Contains(rs.Warnings[0], `"frontend" does not exist`) {
			t.Errorf("got missing=%v warnings=%v", rs.Missing, rs.Warnings)
		}
	})
}

func TestResolveStacks_VarsAndEnv(t *testing.T) {
	rs := resolveFixture(t, map[string]string{"yarn.lock": ""}, "web")
	if rs.Vars["pm"] != "yarn" {
		t.Errorf("pm = %q, want yarn", rs.Vars["pm"])
	}
	if rs.Env["CI"] != "true" {
		t.Errorf("env CI = %q, want true", rs.Env["CI"])
	}
}
