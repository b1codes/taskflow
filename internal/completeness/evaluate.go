package completeness

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
	Subject  string
	Source   string
	Severity string
	Describe string
	Expected []string // rendered paths/globs, relative to the project
	// Satisfied means the requirement is met: its checks passed and it needs
	// no attestation.
	Satisfied bool
	// ChecksPassed means the checks taskflow can run passed (or there are
	// none). With an attestation on record, that makes the requirement met.
	ChecksPassed bool
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
	PlanHints []string
	// PlanVars are the vars tf_plan needs for the rule, when it is plannable.
	PlanVars []string
	// Requires reads like "story (required)" per requirement.
	Requires []string
}

// Recheck names an obligation found by an earlier evaluation, by rule and
// subject (relative to the project).
type Recheck struct {
	RuleID  string
	Subject string
	// Declared subjects (planned with tf_plan) are checked even when the file
	// doesn't exist (yet). Others, inherited ones included, are checked only
	// while it exists: their trigger fired on the file, so once it is gone the
	// work they named is too.
	Declared bool
	// SubjectFile adds the subject-file requirement: the planned file itself
	// must exist. File-scoped rules only.
	SubjectFile bool
}

// DroppedRecheck is a recheck the manifest no longer applies to, so its
// obligations can't be found again whatever changes.
type DroppedRecheck struct {
	Recheck
	// Reason says why, for warnings.
	Reason string
}

type Result struct {
	Stacks   []ResolvedStack
	Rules    []RuleSummary
	Findings []Finding
	// Dropped are the rechecks that can no longer apply: their rule left the
	// manifest, its stack is missing, or the subject is outside the stack
	// root or no longer fits the rule. It is decided by the manifest alone,
	// never by the change set.
	Dropped  []DroppedRecheck
	Warnings []string
}

// noCommand fills {command} when no command resolved for the stack.
const noCommand = "the project's test suite"

// SubjectFileRequirement is the requirement id of the obligation tf_plan adds
// for a planned subject itself: the file must come to exist. Manifests can't
// use it.
const SubjectFileRequirement = "subject-file"

// commandsExecuted reports whether command checks (run:) are executed. They
// aren't yet, so an attest_when: no_command attestation always stands in for
// the command it can't run.
const commandsExecuted = false

// Evaluate checks changes against m. Paths in changes are relative to
// projectPath. Command checks (run:) are not executed, so a requirement with
// an attest prompt always needs an attestation: for attest_when: no_command
// the attestation stands in for the command it can't run yet.
//
// recheck lists earlier findings to verify again even when their subject
// isn't in changes; a recheck that still applies is reported as found, so the
// caller won't void it. Declared subjects are checked even when the file
// doesn't exist, others only while it does. A recheck the manifest can no
// longer apply to at all is reported in Result.Dropped instead, so the caller
// can void its obligations whatever the change set.
func Evaluate(projectPath string, m *Manifest, o *Overrides, changes []Change, recheck []Recheck) *Result {
	res := &Result{Stacks: ResolveStacks(projectPath, m, o)}
	stacks := map[string]*ResolvedStack{}
	for i := range res.Stacks {
		stacks[res.Stacks[i].ID] = &res.Stacks[i]
		res.Warnings = append(res.Warnings, res.Stacks[i].Warnings...)
	}

	sorted := append([]Change(nil), changes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	rechecks := map[string][]Recheck{}
	for _, rc := range recheck {
		rechecks[rc.RuleID] = append(rechecks[rc.RuleID], rc)
	}
	drop := func(rc Recheck, reason string) {
		res.Dropped = append(res.Dropped, DroppedRecheck{Recheck: rc, Reason: reason})
	}
	cache := newFSCache()

	for i := range m.Rules {
		r := &m.Rules[i]
		res.Rules = append(res.Rules, summarize(r))

		root := "."
		var rs *ResolvedStack
		if r.Stack != "" {
			rs = stacks[r.Stack]
			if rs.Missing {
				for _, rc := range rechecks[r.ID] {
					drop(rc, fmt.Sprintf("stack %s is missing", r.Stack))
				}
				continue
			}
			root = rs.Root
		}
		c := &checker{
			projectPath: projectPath,
			root:        root,
			rootAbs:     filepath.Join(projectPath, filepath.FromSlash(root)),
			changes:     relativeTo(root, sorted),
			cache:       cache,
		}
		base := map[string]string{"stack": r.Stack, "root": root}

		if r.Scope == ScopeStack {
			subject := r.Stack
			if subject == "" {
				subject = "*"
			}
			fired := false
			for _, rc := range rechecks[r.ID] {
				if rc.Subject == subject {
					fired = true
				} else {
					// The rule's stack, or its scope, changed since.
					drop(rc, fmt.Sprintf("the rule now applies to %q", subject))
				}
			}
			for _, ch := range c.changes {
				if _, ok := r.match(ch); ok {
					fired = true
					break
				}
			}
			if fired {
				res.Findings = append(res.Findings, c.check(r, rs, subject, "", base)...)
			}
			continue
		}
		checked := map[string]bool{}
		fileFindings := func(rel string, caps map[string]string) {
			checked[rel] = true
			vars := builtinVars(rel)
			for k, v := range base {
				vars[k] = v
			}
			for k, v := range caps {
				vars[k] = v
			}
			res.Findings = append(res.Findings, c.check(r, rs, c.projectRel(rel), rel, vars)...)
		}
		for _, ch := range c.changes {
			if caps, ok := r.match(ch); ok {
				fileFindings(ch.Path, caps)
			}
		}
		// A recheck's trigger fired in an earlier evaluation; it still applies
		// while the file fits the rule and, unless it was declared, exists.
		for _, rc := range rechecks[r.ID] {
			rel, ok := c.rootRel(rc.Subject)
			if !ok {
				drop(rc, fmt.Sprintf("it is outside stack %s's root %q", r.Stack, root))
				continue
			}
			caps, ok := r.matchPath(rel)
			if !ok {
				drop(rc, "it no longer fits the rule's triggers")
				continue
			}
			if rc.SubjectFile {
				res.Findings = append(res.Findings, c.subjectFile(r, rc.Subject, rel, caps, base))
			}
			if checked[rel] {
				continue
			}
			// No trigger fires on a deleted file (deleted: is parsed but not
			// acted on, and changed: skips deletions), so an undeclared
			// subject existed when it was found, and once it's gone so is the
			// work. Acting on deleted: triggers would need their subjects
			// checked without the file.
			if !rc.Declared {
				if _, err := statInRoot(c.rootAbs, rel); err != nil {
					continue
				}
			}
			fileFindings(rel, caps)
		}
	}
	for _, rc := range recheck {
		if !slices.ContainsFunc(res.Rules, func(r RuleSummary) bool { return r.ID == rc.RuleID }) {
			drop(rc, "the rule is no longer in the manifest")
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

// matchPath reports whether path fits one of r's acted-on triggers whatever
// its change type, with the captures.
func (r *Rule) matchPath(p string) (map[string]string, bool) {
	for _, ex := range r.exclude {
		if _, ok := ex.Match(p); ok {
			return nil, false
		}
	}
	for _, t := range r.triggers {
		switch t.kind {
		case "added", "modified", "changed":
			if caps, ok := t.pattern.Match(p); ok {
				return caps, true
			}
		}
	}
	return nil, false
}

func summarize(r *Rule) RuleSummary {
	s := RuleSummary{ID: r.ID, Describe: r.Describe, Stack: r.Stack, Plannable: r.Plannable, PlanHints: r.PlanHints}
	if p := r.planPattern(); r.Plannable && p != nil {
		s.PlanVars = p.Vars()
	}
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
	cache       *fsCache
}

func (c *checker) projectRel(rel string) string {
	if c.root == "." {
		return rel
	}
	return path.Join(c.root, rel)
}

// rootRel is the inverse of projectRel: p relative to the checker's root.
func (c *checker) rootRel(p string) (string, bool) {
	if c.root == "." {
		return p, true
	}
	return strings.CutPrefix(p, c.root+"/")
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
		f.ChecksPassed = f.Satisfied
		if needsAttestation(q, rs) {
			f.AttestPrompt, _ = render(q.Attest, qvars)
			f.Satisfied = false
		}
		out = append(out, f)
	}
	return out
}

// subjectFile is the subject-file finding for a planned subject: the file
// itself must exist. rel is the subject relative to the checker's root.
func (c *checker) subjectFile(r *Rule, subject, rel string, caps, base map[string]string) Finding {
	vars := builtinVars(rel)
	for k, v := range base {
		vars[k] = v
	}
	for k, v := range caps {
		vars[k] = v
	}
	describe, _ := render(r.Describe, vars)
	f := Finding{
		RuleID:        r.ID,
		RequirementID: SubjectFileRequirement,
		Subject:       subject,
		Source:        r.Source,
		Severity:      SeverityRequired,
		Describe:      describe + ": " + path.Base(rel),
		Expected:      []string{subject},
	}
	if _, err := statInRoot(c.rootAbs, rel); err == nil {
		f.Satisfied, f.ChecksPassed, f.Levels = true, true, []string{"structural"}
	}
	return f
}

// needsAttestation reports whether q needs an attestation to be met:
// always for attest_when: always, and for no_command while its command check
// can't run (commands aren't executed, or none resolved for the stack).
func needsAttestation(q *Requirement, rs *ResolvedStack) bool {
	if q.Attest == "" {
		return false
	}
	if q.AttestWhen != AttestNoCommand {
		return true
	}
	return !commandsExecuted || q.Run == nil || commandFor(q, rs, nil) == noCommand
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
		pat, missing := renderPath(v.Exists, vars)
		if len(missing) > 0 {
			return false
		}
		matches, err := c.cache.glob(c.rootAbs, pat)
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
			if contentMM && !c.minMatches(text, v.MinMatches) {
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
		file, missing := renderPath(ci.File, vars)
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
		file, missing := renderPath(mm.File, vars)
		text, ok := c.read(file)
		if len(missing) > 0 || !ok || !c.minMatches(text, mm) {
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
	pat, missing := renderPath(tmpl, vars)
	if len(missing) > 0 {
		return nil
	}
	p, err := c.cache.pattern(pat)
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
			p, _ := renderPath(tmpl, vars)
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
	return c.cache.read(c.rootAbs, rel)
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

func (c *checker) minMatches(text string, mm *MinMatches) bool {
	re := c.cache.regexp(mm.Pattern)
	if re == nil {
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
