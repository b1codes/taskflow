// Package completeness reads a project's completeness manifest and checks a
// set of changed files against it. It touches only the project's files: the
// changed set is supplied by the caller (see internal/adapters), and nothing
// is persisted here.
package completeness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest locations, relative to the project path. The first one found wins.
var ManifestPaths = []string{
	".agents/completeness.yaml",   // written by b1 pair
	".taskflow/completeness.yaml", // hand-written, for projects without b1
}

// OverridesPath holds project-level stack vars and pinned commands.
const OverridesPath = ".agents/project/completeness.yaml"

type Manifest struct {
	Version int      `yaml:"version"`
	Gate    string   `yaml:"gate"`
	Sources []string `yaml:"sources"`
	Stacks  []Stack  `yaml:"stacks"`
	Rules   []Rule   `yaml:"rules"`

	// Path is the manifest's location relative to the project.
	Path string `yaml:"-"`
}

type Stack struct {
	ID     string            `yaml:"id"`
	Source string            `yaml:"source"`
	Root   string            `yaml:"root"`
	Env    map[string]string `yaml:"env"`
	// Vars and Commands list candidates per name; the first whose condition
	// holds wins.
	Vars     map[string][]Candidate `yaml:"vars"`
	Commands map[string][]Candidate `yaml:"commands"`
}

type Candidate struct {
	If    *Condition `yaml:"if"`
	Value string     `yaml:"value"` // for vars
	Run   string     `yaml:"run"`   // for commands
}

// Condition is evaluated against the stack root. An empty Condition (or a
// candidate with no "if") always holds.
type Condition struct {
	Exists       string        `yaml:"exists"`
	HasScript    string        `yaml:"has_script"`
	FileContains *FileContains `yaml:"file_contains"`
	Any          []Condition   `yaml:"any"`
	All          []Condition   `yaml:"all"`
}

type FileContains struct {
	File string `yaml:"file"`
	Text string `yaml:"text"`
}

type Rule struct {
	ID        string        `yaml:"id"`
	Source    string        `yaml:"source"`
	Stack     string        `yaml:"stack"`
	Describe  string        `yaml:"describe"`
	When      Trigger       `yaml:"when"`
	Scope     string        `yaml:"scope"` // file (default) | stack
	Plannable bool          `yaml:"plannable"`
	PlanHints []string      `yaml:"plan_hints"`
	Require   []Requirement `yaml:"require"`

	triggers []trigger
	exclude  []*Pattern
}

// Trigger kinds a rule fires on. Deleted fires on a file removed since the
// base commit (or the old path of a rename), so a rule can ask for what was
// built around the file to go too.
type Trigger struct {
	Added    string   `yaml:"added"`
	Modified string   `yaml:"modified"`
	Changed  string   `yaml:"changed"`
	Deleted  string   `yaml:"deleted"`
	Exclude  []string `yaml:"exclude"`
}

type trigger struct {
	kind    string // added | modified | changed | deleted
	pattern *Pattern
}

const (
	ScopeFile  = "file"
	ScopeStack = "stack"

	SeverityRequired    = "required"
	SeverityRecommended = "recommended"

	AttestAlways    = "always"
	AttestNoCommand = "no_command"
)

type Requirement struct {
	ID         string  `yaml:"id"`
	Source     string  `yaml:"source"`
	Severity   string  `yaml:"severity"`
	Describe   string  `yaml:"describe"`
	OnlyIf     *Verify `yaml:"only_if"`
	Verify     *Verify `yaml:"verify"`
	Attest     string  `yaml:"attest"`
	AttestWhen string  `yaml:"attest_when"`
	Run        *Run    `yaml:"run"`
}

// Verify is a set of file checks; every key present must pass.
type Verify struct {
	Exists string `yaml:"exists"`
	// Absent passes when nothing matches: e.g. a deleted screen's stories
	// are gone too.
	Absent      string      `yaml:"absent"`
	Changed     string      `yaml:"changed"`
	Contains    []string    `yaml:"contains"`
	ContainsAny []string    `yaml:"contains_any"`
	ContainsIn  *ContainsIn `yaml:"contains_in"`
	MinMatches  *MinMatches `yaml:"min_matches"`
	AnyOf       []Verify    `yaml:"any_of"`
	AllOf       []Verify    `yaml:"all_of"`
}

// ContainsIn checks a named file rather than the one found by exists/changed.
type ContainsIn struct {
	File string   `yaml:"file"`
	Any  []string `yaml:"any"`
	All  []string `yaml:"all"`
}

// MinMatches requires at least Count matches of the regexp Pattern, in File
// or else in the file found by exists/changed.
type MinMatches struct {
	Pattern string `yaml:"pattern"`
	Count   int    `yaml:"count"`
	File    string `yaml:"file"`
}

// Run is an optional command check: a named stack command with extra args,
// or a literal command line (written as a plain string).
type Run struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Literal string   `yaml:"-"`
}

func (r *Run) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&r.Literal)
	}
	type plain Run
	return n.Decode((*plain)(r))
}

// Overrides is the part of .agents/project/completeness.yaml taskflow reads:
// per-stack vars and pinned commands, keyed by stack id.
type Overrides struct {
	Stacks map[string]StackOverride `yaml:"stacks"`
}

type StackOverride struct {
	Vars     map[string]string `yaml:"vars"`
	Commands map[string]string `yaml:"commands"`
}

// ManifestError reports a manifest that exists but can't be used.
type ManifestError struct {
	Path     string
	Problems []string
}

func (e *ManifestError) Error() string {
	return fmt.Sprintf("invalid completeness manifest %s: %s", e.Path, strings.Join(e.Problems, "; "))
}

// Load reads the project's manifest. It returns nil, nil when there is none,
// and a *ManifestError when one exists but is unreadable or invalid.
func Load(projectPath string) (*Manifest, error) {
	for _, rel := range ManifestPaths {
		data, err := readInRoot(projectPath, rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, &ManifestError{Path: rel, Problems: []string{err.Error()}}
		}
		m, err := Parse(data)
		if err != nil {
			var me *ManifestError
			if errors.As(err, &me) {
				me.Path = rel
				return nil, me
			}
			return nil, &ManifestError{Path: rel, Problems: []string{err.Error()}}
		}
		m.Path = rel
		return m, nil
	}
	return nil, nil
}

// Parse decodes and validates a manifest. Unknown keys are errors, so a typo
// like "requires:" can't silently disable a rule.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &ManifestError{Problems: []string{"manifest is empty"}}
		}
		return nil, &ManifestError{Problems: []string{err.Error()}}
	}
	if problems := m.validate(); len(problems) > 0 {
		return nil, &ManifestError{Problems: problems}
	}
	return &m, nil
}

// LoadOverrides reads the project override file; a missing file is empty.
// Other keys in the file (b1's rule overrides) are ignored.
func LoadOverrides(projectPath string) (*Overrides, error) {
	data, err := readInRoot(projectPath, OverridesPath)
	if errors.Is(err, os.ErrNotExist) {
		return &Overrides{}, nil
	}
	if err != nil {
		return &Overrides{}, err
	}
	var o Overrides
	if err := yaml.Unmarshal(data, &o); err != nil {
		return &Overrides{}, fmt.Errorf("%s: %w", OverridesPath, err)
	}
	return &o, nil
}

func (m *Manifest) stack(id string) *Stack {
	for i := range m.Stacks {
		if m.Stacks[i].ID == id {
			return &m.Stacks[i]
		}
	}
	return nil
}

// validate fills defaults, compiles trigger patterns, and returns every
// problem found, each prefixed with its location in the file.
func (m *Manifest) validate() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if m.Version != 1 {
		add("version: must be 1, got %d", m.Version)
	}
	// The gate names mirror session.GateMode; this package stays free of a
	// session import, and session treats any name it doesn't know as block.
	switch m.Gate {
	case "", "report", "review", "block":
	default:
		add("gate: must be report, review, or block, got %q", m.Gate)
	}

	stackIDs := map[string]bool{}
	for i := range m.Stacks {
		s := &m.Stacks[i]
		at := fmt.Sprintf("stacks[%d]", i)
		if s.ID == "" {
			add("%s.id: required", at)
		} else if stackIDs[s.ID] {
			add("%s.id: duplicate stack %q", at, s.ID)
		}
		stackIDs[s.ID] = true
		// Clean so "./web" or "web/" match changed paths, which are relative
		// to the project with no "./" prefix or trailing slash.
		s.Root = path.Clean(s.Root)
		if !filepath.IsLocal(filepath.FromSlash(s.Root)) {
			add("%s.root: must be a path inside the project, got %q", at, s.Root)
		}
		for name, cands := range s.Vars {
			for j, c := range cands {
				validateCondition(c.If, fmt.Sprintf("%s.vars.%s[%d].if", at, name, j), add)
			}
		}
		for name, cands := range s.Commands {
			for j, c := range cands {
				if c.Run == "" {
					add("%s.commands.%s[%d].run: required", at, name, j)
				}
				validateCondition(c.If, fmt.Sprintf("%s.commands.%s[%d].if", at, name, j), add)
			}
		}
	}

	ruleIDs := map[string]bool{}
	for i := range m.Rules {
		r := &m.Rules[i]
		at := fmt.Sprintf("rules[%d]", i)
		if r.ID == "" {
			add("%s.id: required", at)
		} else {
			at = fmt.Sprintf("rules[%s]", r.ID)
			if ruleIDs[r.ID] {
				add("%s: duplicate rule id", at)
			}
		}
		ruleIDs[r.ID] = true
		if r.Stack != "" && !stackIDs[r.Stack] {
			add("%s.stack: no stack with id %q", at, r.Stack)
		}
		switch r.Scope {
		case "":
			r.Scope = ScopeFile
		case ScopeFile, ScopeStack:
		default:
			add("%s.scope: must be file or stack, got %q", at, r.Scope)
		}

		r.triggers = nil
		for _, t := range []struct{ kind, src string }{
			{"added", r.When.Added}, {"modified", r.When.Modified},
			{"changed", r.When.Changed}, {"deleted", r.When.Deleted},
		} {
			if t.src == "" {
				continue
			}
			p, err := CompilePattern(t.src)
			if err != nil {
				add("%s.when.%s: %v", at, t.kind, err)
				continue
			}
			r.triggers = append(r.triggers, trigger{kind: t.kind, pattern: p})
		}
		if r.When.Added == "" && r.When.Modified == "" && r.When.Changed == "" && r.When.Deleted == "" {
			add("%s.when: needs one of added, modified, changed, deleted", at)
		}
		r.exclude = nil
		for j, ex := range r.When.Exclude {
			p, err := CompilePattern(ex)
			if err != nil {
				add("%s.when.exclude[%d]: %v", at, j, err)
				continue
			}
			r.exclude = append(r.exclude, p)
		}

		known := r.knownVars()
		if len(r.Require) == 0 {
			add("%s.require: at least one requirement is needed", at)
		}
		reqIDs := map[string]bool{}
		for j := range r.Require {
			q := &r.Require[j]
			qat := fmt.Sprintf("%s.require[%d]", at, j)
			if q.ID == "" {
				add("%s.id: required", qat)
			} else {
				qat = fmt.Sprintf("%s.require[%s]", at, q.ID)
				if reqIDs[q.ID] {
					add("%s: duplicate requirement id", qat)
				}
				if q.ID == SubjectFileRequirement {
					add("%s.id: %q is reserved for planned subject files", qat, q.ID)
				}
			}
			reqIDs[q.ID] = true
			switch q.Severity {
			case "":
				q.Severity = SeverityRequired
			case SeverityRequired, SeverityRecommended:
			default:
				add("%s.severity: must be required or recommended, got %q", qat, q.Severity)
			}
			switch q.AttestWhen {
			case "":
				q.AttestWhen = AttestAlways
			case AttestAlways, AttestNoCommand:
			default:
				add("%s.attest_when: must be always or no_command, got %q", qat, q.AttestWhen)
			}
			if q.Describe == "" {
				add("%s.describe: required", qat)
			}
			if q.Verify == nil && q.Attest == "" && q.Run == nil {
				add("%s: needs verify, attest, or run", qat)
			}
			if q.Run != nil {
				if q.Run.Literal == "" && q.Run.Command == "" {
					add("%s.run: needs a command name or a command line", qat)
				}
				if q.Run.Command != "" && r.Stack == "" {
					add("%s.run.command: the rule has no stack to take %q from", qat, q.Run.Command)
				}
				for _, a := range q.Run.Args {
					for _, v := range templateVars(a) {
						if !known[v] {
							add("%s.run.args: unknown variable {%s}", qat, v)
						}
					}
				}
			}
			checkVars := func(loc, tmpl string) {
				for _, v := range templateVars(tmpl) {
					if !known[v] {
						add("%s: unknown variable {%s}", loc, v)
					}
				}
			}
			checkVars(qat+".describe", q.Describe)
			checkVars(qat+".attest", q.Attest)
			validateVerify(q.OnlyIf, qat+".only_if", checkVars, add)
			validateVerify(q.Verify, qat+".verify", checkVars, add)
		}
		for _, v := range templateVars(r.Describe) {
			if !known[v] {
				add("%s.describe: unknown variable {%s}", at, v)
			}
		}
	}
	return problems
}

// knownVars returns the vars templates in r may use: captures shared by all
// of its trigger patterns plus the built-ins.
func (r *Rule) knownVars() map[string]bool {
	known := map[string]bool{"stack": true, "root": true, "command": true}
	if r.Scope == ScopeStack {
		return known
	}
	for _, b := range []string{"path", "dir", "stem"} {
		known[b] = true
	}
	var shared map[string]bool
	for _, t := range r.triggers {
		these := map[string]bool{}
		for _, v := range t.pattern.Vars() {
			if shared == nil || shared[v] {
				these[v] = true
			}
		}
		shared = these
	}
	for v := range shared {
		known[v] = true
	}
	return known
}

func validateCondition(c *Condition, at string, add func(string, ...any)) {
	if c == nil {
		return
	}
	if c.Exists != "" {
		if _, err := CompilePattern(c.Exists); err != nil {
			add("%s.exists: %v", at, err)
		}
	}
	if c.FileContains != nil {
		if c.FileContains.File == "" || c.FileContains.Text == "" {
			add("%s.file_contains: needs file and text", at)
		} else if _, err := CompilePattern(c.FileContains.File); err != nil {
			add("%s.file_contains.file: %v", at, err)
		}
	}
	for i := range c.Any {
		validateCondition(&c.Any[i], fmt.Sprintf("%s.any[%d]", at, i), add)
	}
	for i := range c.All {
		validateCondition(&c.All[i], fmt.Sprintf("%s.all[%d]", at, i), add)
	}
}

func validateVerify(v *Verify, at string, checkVars func(loc, tmpl string), add func(string, ...any)) {
	if v == nil {
		return
	}
	pattern := func(field, src string) {
		if src == "" {
			return
		}
		checkVars(at+"."+field, src)
		if _, err := CompilePattern(src); err != nil {
			add("%s.%s: %v", at, field, err)
		}
	}
	pattern("exists", v.Exists)
	pattern("absent", v.Absent)
	pattern("changed", v.Changed)
	for _, s := range v.Contains {
		checkVars(at+".contains", s)
	}
	for _, s := range v.ContainsAny {
		checkVars(at+".contains_any", s)
	}
	if v.ContainsIn != nil {
		if v.ContainsIn.File == "" || (len(v.ContainsIn.Any) == 0 && len(v.ContainsIn.All) == 0) {
			add("%s.contains_in: needs file and any or all", at)
		}
		pattern("contains_in.file", v.ContainsIn.File)
		for _, s := range append(append([]string{}, v.ContainsIn.Any...), v.ContainsIn.All...) {
			checkVars(at+".contains_in", s)
		}
	}
	if v.MinMatches != nil {
		if _, err := regexp.Compile(v.MinMatches.Pattern); err != nil || v.MinMatches.Pattern == "" {
			add("%s.min_matches.pattern: must be a regular expression", at)
		}
		if v.MinMatches.Count < 1 {
			add("%s.min_matches.count: must be at least 1", at)
		}
		pattern("min_matches.file", v.MinMatches.File)
	}
	for i := range v.AnyOf {
		validateVerify(&v.AnyOf[i], fmt.Sprintf("%s.any_of[%d]", at, i), checkVars, add)
	}
	for i := range v.AllOf {
		validateVerify(&v.AllOf[i], fmt.Sprintf("%s.all_of[%d]", at, i), checkVars, add)
	}
}
