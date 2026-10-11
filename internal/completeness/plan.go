package completeness

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// PlannedSubject is a tf_plan item resolved against the manifest.
type PlannedSubject struct {
	RuleID string
	// Subject is the planned file (relative to the project), or for a
	// stack-scoped rule the stack id ("*" when the rule has no stack).
	Subject string
}

// PlanSubject resolves a tf_plan item: the subject that rule ruleID would
// fire on, rendered from vars. Only plannable rules can be planned. A
// file-scoped rule needs a trigger without wildcards, and vars must give
// exactly its captures, each one path-segment fragment.
func PlanSubject(m *Manifest, ruleID string, vars map[string]string) (*PlannedSubject, error) {
	var r *Rule
	for i := range m.Rules {
		if m.Rules[i].ID == ruleID {
			r = &m.Rules[i]
		}
	}
	switch {
	case r == nil:
		return nil, fmt.Errorf("unknown rule %q (plannable rules: %s)", ruleID, orNone(m.plannableRules()))
	case !r.Plannable:
		return nil, fmt.Errorf("rule %q is not plannable (plannable rules: %s)", ruleID, orNone(m.plannableRules()))
	}

	if r.Scope == ScopeStack {
		if len(vars) > 0 {
			return nil, fmt.Errorf("rule %q is planned per stack and takes no vars", ruleID)
		}
		subject := r.Stack
		if subject == "" {
			subject = "*"
		}
		return &PlannedSubject{RuleID: r.ID, Subject: subject}, nil
	}

	p := r.planPattern()
	if p == nil {
		return nil, fmt.Errorf("rule %q can't be planned: each of its triggers has wildcards, so there is no single file to plan", ruleID)
	}
	want := p.Vars()
	var missing, unknown []string
	for _, v := range want {
		if _, ok := vars[v]; !ok {
			missing = append(missing, v)
		}
	}
	for k, v := range vars {
		if !slices.Contains(want, k) {
			unknown = append(unknown, k)
			continue
		}
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
			return nil, fmt.Errorf("rule %q: var %s=%q must be a non-empty file or directory name, without slashes", ruleID, k, v)
		}
	}
	slices.Sort(unknown)
	switch {
	case len(missing) > 0:
		return nil, fmt.Errorf("rule %q needs vars %s (missing %s)", ruleID, strings.Join(want, ", "), strings.Join(missing, ", "))
	case len(unknown) > 0:
		return nil, fmt.Errorf("rule %q takes vars %s, not %s", ruleID, strings.Join(want, ", "), strings.Join(unknown, ", "))
	}

	rel, _ := renderPath(p.String(), vars)
	if _, ok := r.matchPath(rel); !ok {
		return nil, fmt.Errorf("rule %q doesn't apply to %s (excluded)", ruleID, rel)
	}
	root := "."
	if s := m.stack(r.Stack); s != nil {
		root = s.Root
	}
	subject := rel
	if root != "." {
		subject = path.Join(root, rel)
	}
	return &PlannedSubject{RuleID: r.ID, Subject: subject}, nil
}

// planPattern is the trigger a planned subject is rendered from: the first
// added, modified, or changed pattern with no wildcards, so vars alone name
// one file. Nil when there is none.
func (r *Rule) planPattern() *Pattern {
	for _, t := range r.triggers {
		if t.kind != "deleted" && !hasGlob(t.pattern.String()) {
			return t.pattern
		}
	}
	return nil
}

func (m *Manifest) plannableRules() []string {
	var out []string
	for _, r := range m.Rules {
		if r.Plannable {
			out = append(out, r.ID)
		}
	}
	return out
}

func orNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
