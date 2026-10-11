package completeness

import (
	"errors"
	"strings"
	"testing"
)

const minimalRule = `
version: 1
rules:
  - id: r
    when: { added: "src/{name}.ts" }
    require:
      - id: q
        describe: "Test for {name}"
        verify: { exists: "src/{name}.test.ts" }
`

func TestParse_Defaults(t *testing.T) {
	m, err := Parse([]byte(minimalRule + "stacks:\n  - id: web\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Rules[0].Scope != ScopeFile {
		t.Errorf("scope default = %q, want file", m.Rules[0].Scope)
	}
	q := m.Rules[0].Require[0]
	if q.Severity != SeverityRequired || q.AttestWhen != AttestAlways {
		t.Errorf("requirement defaults = %q/%q", q.Severity, q.AttestWhen)
	}
	if m.Stacks[0].Root != "." {
		t.Errorf("stack root default = %q, want .", m.Stacks[0].Root)
	}
}

func TestParse_Run(t *testing.T) {
	m, err := Parse([]byte(`
version: 1
stacks: [{ id: web }]
rules:
  - id: r
    stack: web
    when: { changed: "**" }
    scope: stack
    require:
      - { id: named, describe: "d", run: { command: test, args: ["tests/contract"] } }
      - { id: literal, describe: "d", run: "make check" }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	named, literal := m.Rules[0].Require[0].Run, m.Rules[0].Require[1].Run
	if named.Command != "test" || len(named.Args) != 1 || named.Literal != "" {
		t.Errorf("named run = %+v", named)
	}
	if literal.Literal != "make check" || literal.Command != "" {
		t.Errorf("literal run = %+v", literal)
	}
}

func TestParse_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"wrong version", strings.Replace(minimalRule, "version: 1", "version: 2", 1), "version: must be 1"},
		{"typo key", strings.Replace(minimalRule, "require:", "requires:", 1), "field requires not found"},
		{"bad gate", minimalRule + "gate: strict\n", "gate: must be report, review, or block"},
		{"unknown stack", strings.Replace(minimalRule, "  - id: r\n", "  - id: r\n    stack: web\n", 1), `rules[r].stack: no stack with id "web"`},
		{"unknown variable", strings.Replace(minimalRule, "{name}.test.ts", "{nam}.test.ts", 1), "rules[r].require[q].verify.exists: unknown variable {nam}"},
		{"bad severity", strings.Replace(minimalRule, "describe:", "severity: must\n        describe:", 1), "severity: must be required or recommended"},
		{"no trigger", strings.Replace(minimalRule, `when: { added: "src/{name}.ts" }`, "when: { exclude: [x] }", 1), "rules[r].when: needs one of"},
		{"duplicate rule", minimalRule + strings.TrimPrefix(minimalRule[strings.Index(minimalRule, "  - id: r"):], ""), "duplicate rule id"},
		{"bad min_matches", strings.Replace(minimalRule, `verify: { exists: "src/{name}.test.ts" }`, `verify: { min_matches: { pattern: "(", count: 1 } }`, 1), "min_matches.pattern"},
		{"run without stack", strings.Replace(minimalRule, `verify: { exists: "src/{name}.test.ts" }`, `run: { command: test }`, 1), "the rule has no stack"},
		{"stack scope has no captures", strings.Replace(minimalRule, "  - id: r\n", "  - id: r\n    scope: stack\n", 1), "unknown variable {name}"},
		{"root escapes project", minimalRule + "stacks:\n  - id: web\n    root: ../elsewhere\n", "must be a path inside the project"},
		{"verify path escapes project", strings.Replace(minimalRule, `verify: { exists: "src/{name}.test.ts" }`, `verify: { contains_in: { file: "../secrets.txt", any: [x] } }`, 1), "must stay inside the project"},
		{"condition path escapes project", minimalRule + "stacks:\n  - id: web\n    commands:\n      test:\n        - { if: { file_contains: { file: \"../../.ssh/config\", text: x } }, run: t }\n", "file_contains.file"},
		{"empty", "", "manifest is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			var me *ManifestError
			if !errors.As(err, &me) {
				t.Fatalf("expected *ManifestError, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("no manifest", func(t *testing.T) {
		m, err := Load(t.TempDir())
		if m != nil || err != nil {
			t.Errorf("Load = %v, %v; want nil, nil", m, err)
		}
	})
	t.Run("hand-written fallback", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{".taskflow/completeness.yaml": minimalRule})
		m, err := Load(root)
		if err != nil || m == nil || m.Path != ".taskflow/completeness.yaml" {
			t.Fatalf("Load = %+v, %v", m, err)
		}
	})
	t.Run("b1 manifest wins", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{
			".taskflow/completeness.yaml": "not: [valid",
			".agents/completeness.yaml":   minimalRule,
		})
		m, err := Load(root)
		if err != nil || m == nil || m.Path != ".agents/completeness.yaml" {
			t.Fatalf("Load = %+v, %v", m, err)
		}
	})
	t.Run("invalid manifest names its path", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{".agents/completeness.yaml": "version: 1\nrules: [{ id: r }]\n"})
		_, err := Load(root)
		var me *ManifestError
		if !errors.As(err, &me) || me.Path != ".agents/completeness.yaml" {
			t.Fatalf("Load error = %v", err)
		}
	})
}
