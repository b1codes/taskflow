package adapters

import (
	"context"
	"errors"
	"strings"

	"github.com/b1codes/taskflow/internal/completeness"
	"github.com/b1codes/taskflow/internal/gitctx"
	"github.com/b1codes/taskflow/internal/session"
)

// CompletenessEvaluator implements session.CompletenessEvaluator using
// internal/completeness for the rules and internal/gitctx for the changes.
type CompletenessEvaluator struct{}

// NewCompletenessEvaluator creates a new session.CompletenessEvaluator adapter.
func NewCompletenessEvaluator() *CompletenessEvaluator {
	return &CompletenessEvaluator{}
}

// BaseSHA returns the project's HEAD commit, or the empty tree in a repo with
// no commits.
func (e *CompletenessEvaluator) BaseSHA(projectPath string) (string, error) {
	return gitctx.HeadSHA(projectPath)
}

// Evaluate re-reads the project's manifest and checks the session's changes
// since its base commit against it. It returns nil, nil when the project has
// no manifest, and an Evaluation carrying Error when the manifest is invalid.
func (e *CompletenessEvaluator) Evaluate(ctx context.Context, sess *session.Session) (*session.Evaluation, error) {
	m, err := completeness.Load(sess.ProjectPath)
	if err != nil {
		var me *completeness.ManifestError
		if errors.As(err, &me) {
			return &session.Evaluation{Manifest: me.Path, Error: strings.Join(me.Problems, "; ")}, nil
		}
		return nil, err
	}
	if m == nil {
		return nil, nil
	}

	ev := &session.Evaluation{Manifest: m.Path}
	overrides, err := completeness.LoadOverrides(sess.ProjectPath)
	if err != nil {
		ev.Warnings = append(ev.Warnings, "ignoring project overrides: "+err.Error())
	}

	var changes []completeness.Change
	cs, err := gitctx.ChangedSince(sess.ProjectPath, sess.BaseSHA)
	if err != nil {
		// Without a changed set no rule can fire; keep what's stored.
		ev.Partial = true
		if errors.Is(err, session.ErrNotAGitRepository) {
			ev.Warnings = append(ev.Warnings, "not a git repository: changed files can't be read, so no new obligations are found")
		} else {
			ev.Warnings = append(ev.Warnings, "git unavailable: "+err.Error())
		}
	} else {
		ev.Partial = cs.Partial
		ev.Warnings = append(ev.Warnings, cs.Warnings...)
		for _, f := range cs.Files {
			changes = append(changes, completeness.Change{Path: f.Path, OldPath: f.OldPath, Type: completeness.ChangeType(f.Type)})
		}
	}

	res := completeness.Evaluate(sess.ProjectPath, m, overrides, changes)
	ev.Warnings = append(ev.Warnings, res.Warnings...)
	for _, rs := range res.Stacks {
		info := session.StackInfo{ID: rs.ID, Root: rs.Root, Commands: map[string]*string{}, Warning: strings.Join(rs.Warnings, "; ")}
		for _, name := range rs.CommandNames() {
			if line := rs.Commands[name]; line != "" {
				info.Commands[name] = &line
			} else {
				info.Commands[name] = nil
			}
		}
		ev.Stacks = append(ev.Stacks, info)
	}
	for _, r := range res.Rules {
		ev.Rules = append(ev.Rules, session.RuleInfo{ID: r.ID, Describe: r.Describe, Stack: r.Stack, Plannable: r.Plannable, Requires: r.Requires})
	}
	for _, f := range res.Findings {
		ev.Findings = append(ev.Findings, session.CompletenessFinding{
			RuleID:        f.RuleID,
			RequirementID: f.RequirementID,
			Subject:       f.Subject,
			Source:        f.Source,
			Severity:      f.Severity,
			Describe:      f.Describe,
			Expected:      f.Expected,
			Satisfied:     f.Satisfied,
			Levels:        f.Levels,
			AttestPrompt:  f.AttestPrompt,
		})
	}
	return ev, nil
}
