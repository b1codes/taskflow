package completeness

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type ChangeType string

const (
	Added    ChangeType = "A"
	Modified ChangeType = "M"
	Deleted  ChangeType = "D"
	Renamed  ChangeType = "R"
)

// Change is one changed file, with paths relative to the project.
type Change struct {
	Path    string
	OldPath string // renames only
	Type    ChangeType
}

// Finding is one requirement applied to one subject: the input to the
// caller's obligation merge.
type Finding struct {
	RuleID        string
	RequirementID string
	// Subject is the trigger file (relative to the project), or for a
	// stack-scoped rule the stack id ("*" when the rule has no stack).
	Subject   string
	Source    string
	Severity  string
	Describe  string
	Expected  []string // rendered paths/globs, relative to the project
	Satisfied bool
	// Levels lists the verify levels that passed ("structural").
	Levels []string
	// AttestPrompt is set when the requirement also needs an attestation,
	// which keeps it from being satisfied by file checks alone.
	AttestPrompt string
}

// RuleSummary describes a rule for reports.
type RuleSummary struct {
	ID        string
	Describe  string
	Stack     string
	Plannable bool
	// Requires reads like "story (required)" per requirement.
	Requires []string
}

type Result struct {
	Stacks   []ResolvedStack
	Rules    []RuleSummary
	Findings []Finding
	Warnings []string
}

// noCommand fills {command} when no command resolved for the stack.
const noCommand = "the project's test suite"

// Evaluate checks changes against m. Paths in changes are relative to
// projectPath. Command checks (run:) are not executed, so a requirement with
// an attest prompt always needs an attestation: for attest_when: no_command
// the attestation stands in for the command it can't run yet.
func Evaluate(projectPath string, m *Manifest, o *Overrides, changes []Change) *Result {
	res := &Result{Stacks: ResolveStacks(projectPath, m, o)}
	stacks := map[string]*ResolvedStack{}
	for i := range res.Stacks {
		stacks[res.Stacks[i].ID] = &res.Stacks[i]
		res.Warnings = append(res.Warnings, res.Stacks[i].Warnings...)
	}

	sorted := append([]Change(nil), changes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	for i := range m.Rules {
		r := &m.Rules[i]
		res.Rules = append(res.Rules, summarize(r))

		root := "."
		var rs *ResolvedStack
		if r.Stack != "" {
			rs = stacks[r.Stack]
			if rs.Missing {
				continue
			}
			root = rs.Root
		}
		c := &checker{
			projectPath: projectPath,
			root:        root,
			rootAbs:     filepath.Join(projectPath, filepath.FromSlash(root)),
			changes:     relativeTo(root, sorted),
			files:       map[string]*string{},
		}
		base := map[string]string{"stack": r.Stack, "root": root}

		if r.Scope == ScopeStack {
			for _, ch := range c.changes {
				if _, ok := r.match(ch); ok {
					subject := r.Stack
					if subject == "" {
						subject = "*"
					}
					res.Findings = append(res.Findings, c.check(r, rs, subject, "", base)...)
					break
				}
			}
			continue
		}
		for _, ch := range c.changes {
			caps, ok := r.match(ch)
			if !ok {
				continue
			}
			vars := builtinVars(ch.Path)
			for k, v := range base {
				vars[k] = v
			}
			for k, v := range caps {
				vars[k] = v
			}
			res.Findings = append(res.Findings, c.check(r, rs, c.projectRel(ch.Path), ch.Path, vars)...)
		}
	}
	return res
}

// match reports whether ch fires one of r's triggers, with its captures.
// Deleted triggers are parsed but not acted on yet.
func (r *Rule) match(ch Change) (map[string]string, bool) {
	for _, ex := range r.exclude {
		if _, ok := ex.Match(ch.Path); ok {
			return nil, false
		}
	}
	for _, t := range r.triggers {
		var fires bool
		switch t.kind {
		case "added":
			// A renamed file is new at its path compared to the base.
			fires = ch.Type == Added || ch.Type == Renamed
		case "modified":
			fires = ch.Type == Modified
		case "changed":
			fires = ch.Type != Deleted
		}
		if !fires {
			continue
		}
		if caps, ok := t.pattern.Match(ch.Path); ok {
			return caps, true
		}
	}
	return nil, false
}

func summarize(r *Rule) RuleSummary {
	s := RuleSummary{ID: r.ID, Describe: r.Describe, Stack: r.Stack, Plannable: r.Plannable}
	for _, q := range r.Require {
		label := q.Severity
		if q.OnlyIf != nil {
			label += ", conditional"
		}
		s.Requires = append(s.Requires, fmt.Sprintf("%s (%s)", q.ID, label))
	}
	return s
}

// relativeTo returns the changes under root, with paths relative to it.
func relativeTo(root string, changes []Change) []Change {
	if root == "." {
		return changes
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	var out []Change
	for _, ch := range changes {
		if rel, ok := strings.CutPrefix(ch.Path, prefix); ok {
			ch.Path = rel
			out = append(out, ch)
		}
	}
	return out
}

// checker evaluates one rule's requirements under one root. Paths it
// handles are relative to that root.
type checker struct {
	projectPath string
	root        string
	rootAbs     string
	changes     []Change
	files       map[string]*string // contents cache; nil = unreadable
}

func (c *checker) projectRel(rel string) string {
	if c.root == "." {
		return rel
	}
	return path.Join(c.root, rel)
}

func (c *checker) check(r *Rule, rs *ResolvedStack, subject, subjectPath string, vars map[string]string) []Finding {
	var out []Finding
	for i := range r.Require {
		q := &r.Require[i]
		qvars := make(map[string]string, len(vars)+1)
		for k, v := range vars {
			qvars[k] = v
		}
		qvars["command"] = commandFor(q, rs, vars)

		if q.OnlyIf != nil && !c.verify(q.OnlyIf, qvars, subjectPath) {
			continue
		}
		describe, _ := render(q.Describe, qvars)
		f := Finding{
			RuleID:        r.ID,
			RequirementID: q.ID,
			Subject:       subject,
			Source:        q.Source,
			Severity:      q.Severity,
			Describe:      describe,
			Satisfied:     true,
		}
		if f.Source == "" {
			f.Source = r.Source
		}
		if q.Verify != nil {
			f.Expected = c.expected(q.Verify, qvars)
			if c.verify(q.Verify, qvars, subjectPath) {
				f.Levels = []string{"structural"}
			} else {
				f.Satisfied = false
			}
		}
		if q.Attest != "" {
			f.AttestPrompt, _ = render(q.Attest, qvars)
			f.Satisfied = false
		}
		out = append(out, f)
	}
	return out
}

// commandFor renders {command}: the requirement's run command (with its
// args rendered and shell-quoted), else the stack's test command.
func commandFor(q *Requirement, rs *ResolvedStack, vars map[string]string) string {
	if q.Run != nil && q.Run.Literal != "" {
		return q.Run.Literal
	}
	name, args := "test", []string(nil)
	if q.Run != nil {
		name, args = q.Run.Command, q.Run.Args
	}
	if rs == nil || rs.Commands[name] == "" {
		return noCommand
	}
	line := rs.Commands[name]
	for _, a := range renderAll(args, vars) {
		line += " " + shellQuote(a)
	}
	return line
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// verify reports whether every check in v passes. Content checks
// (contains, contains_any, min_matches without a file) apply to the files
// found by exists and/or changed, else to the subject file; one file must
// pass all of them.
func (c *checker) verify(v *Verify, vars map[string]string, subjectPath string) bool {
	var targets []string
	haveTargets := false
	if v.Exists != "" {
		pat, missing := render(v.Exists, vars)
		if len(missing) > 0 {
			return false
		}
		matches, err := globFS(c.rootAbs, pat)
		if err != nil || len(matches) == 0 {
			return false
		}
		targets, haveTargets = matches, true
	}
	if v.Changed != "" {
		matches := c.changedMatching(v.Changed, vars)
		if len(matches) == 0 {
			return false
		}
		if haveTargets {
			targets = intersect(targets, matches)
			if len(targets) == 0 {
				return false
			}
		} else {
			targets, haveTargets = matches, true
		}
	}

	contains := renderAll(v.Contains, vars)
	containsAny := renderAll(v.ContainsAny, vars)
	contentMM := v.MinMatches != nil && v.MinMatches.File == ""
	if len(contains) > 0 || len(containsAny) > 0 || contentMM {
		if !haveTargets {
			if subjectPath == "" {
				return false
			}
			targets = []string{subjectPath}
		}
		passed := false
		for _, t := range targets {
			text, ok := c.read(t)
			if !ok || !containsAll(text, contains) || (len(containsAny) > 0 && !containsSome(text, containsAny)) {
				continue
			}
			if contentMM && !minMatches(text, v.MinMatches) {
				continue
			}
			passed = true
			break
		}
		if !passed {
			return false
		}
	}

	if ci := v.ContainsIn; ci != nil {
		file, missing := render(ci.File, vars)
		text, ok := c.read(file)
		if len(missing) > 0 || !ok {
			return false
		}
		if !containsAll(text, renderAll(ci.All, vars)) {
			return false
		}
		if anyOf := renderAll(ci.Any, vars); len(anyOf) > 0 && !containsSome(text, anyOf) {
			return false
		}
	}
	if mm := v.MinMatches; mm != nil && mm.File != "" {
		file, missing := render(mm.File, vars)
		text, ok := c.read(file)
		if len(missing) > 0 || !ok || !minMatches(text, mm) {
			return false
		}
	}
	if len(v.AnyOf) > 0 {
		passed := false
		for i := range v.AnyOf {
			if c.verify(&v.AnyOf[i], vars, subjectPath) {
				passed = true
				break
			}
		}
		if !passed {
			return false
		}
	}
	for i := range v.AllOf {
		if !c.verify(&v.AllOf[i], vars, subjectPath) {
			return false
		}
	}
	return true
}

// changedMatching returns the changed (non-deleted) files matching tmpl.
func (c *checker) changedMatching(tmpl string, vars map[string]string) []string {
	pat, missing := render(tmpl, vars)
	if len(missing) > 0 {
		return nil
	}
	p, err := CompilePattern(pat)
	if err != nil {
		return nil
	}
	var out []string
	for _, ch := range c.changes {
		if ch.Type == Deleted {
			continue
		}
		if _, ok := p.Match(ch.Path); ok {
			out = append(out, ch.Path)
		}
	}
	return out
}

// expected lists the rendered paths and globs v checks, relative to the
// project, for showing the agent where the work is expected.
func (c *checker) expected(v *Verify, vars map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(v *Verify)
	walk = func(v *Verify) {
		for _, tmpl := range []string{v.Exists, v.Changed, fileOf(v.ContainsIn), fileOfMM(v.MinMatches)} {
			if tmpl == "" {
				continue
			}
			p, _ := render(tmpl, vars)
			if p = c.projectRel(p); !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		for i := range v.AnyOf {
			walk(&v.AnyOf[i])
		}
		for i := range v.AllOf {
			walk(&v.AllOf[i])
		}
	}
	walk(v)
	return out
}

func fileOf(ci *ContainsIn) string {
	if ci == nil {
		return ""
	}
	return ci.File
}

func fileOfMM(mm *MinMatches) string {
	if mm == nil {
		return ""
	}
	return mm.File
}

func (c *checker) read(rel string) (string, bool) {
	if cached, ok := c.files[rel]; ok {
		if cached == nil {
			return "", false
		}
		return *cached, true
	}
	data, err := readInRoot(c.rootAbs, rel)
	if err != nil {
		c.files[rel] = nil
		return "", false
	}
	text := string(data)
	c.files[rel] = &text
	return text, true
}

func renderAll(tmpls []string, vars map[string]string) []string {
	out := make([]string, 0, len(tmpls))
	for _, t := range tmpls {
		s, _ := render(t, vars)
		out = append(out, s)
	}
	return out
}

func containsAll(text string, subs []string) bool {
	for _, s := range subs {
		if !strings.Contains(text, s) {
			return false
		}
	}
	return true
}

func containsSome(text string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func minMatches(text string, mm *MinMatches) bool {
	re, err := regexp.Compile(mm.Pattern)
	if err != nil {
		return false
	}
	return len(re.FindAllStringIndex(text, mm.Count)) >= mm.Count
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}
