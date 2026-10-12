package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/completeness"
	"github.com/b1codes/taskflow/internal/gitctx"
	"github.com/b1codes/taskflow/internal/session"
)

// CompletenessEvaluator implements session.CompletenessEvaluator using
// internal/completeness for the rules and internal/gitctx for the changes.
type CompletenessEvaluator struct {
	commands       bool
	commandTimeout time.Duration
}

// NewCompletenessEvaluator creates a new session.CompletenessEvaluator
// adapter, with command checks off.
func NewCompletenessEvaluator() *CompletenessEvaluator {
	return &CompletenessEvaluator{}
}

// WithCommands turns command checks on or off (completeness.allow_commands),
// each limited to timeout (DefaultCommandTimeout when it is 0 or less). Off,
// evaluations carry no command checks and an attest_when: no_command
// attestation stands in for them.
func (e *CompletenessEvaluator) WithCommands(enabled bool, timeout time.Duration) *CompletenessEvaluator {
	e.commands, e.commandTimeout = enabled, timeout
	return e
}

// BaseSHA returns the project's HEAD commit, or the empty tree in a repo with
// no commits.
func (e *CompletenessEvaluator) BaseSHA(projectPath string) (string, error) {
	return gitctx.HeadSHA(projectPath)
}

// Evaluate re-reads the project's manifest and checks the session's changes
// since its base commit against it. It returns nil, nil when the project has
// no manifest, and an Evaluation carrying Error when the manifest is invalid.
// Planned and inherited obligations are checked by subject, whether or not it
// changed: planned ones even before their file exists, inherited ones only
// while it does (so one whose file was deleted or reverted isn't found, and a
// full evaluation voids it). When the changed set is partial, the other stored
// open, satisfied, and attested obligations are verified again too, so work
// finished outside it is still noticed. Rechecked subjects the manifest no
// longer applies to are reported in Evaluation.Dropped.
func (e *CompletenessEvaluator) Evaluate(ctx context.Context, sess *session.Session, stored []*session.Obligation) (*session.Evaluation, error) {
	return e.evaluate(sess, stored, nil)
}

// Plan is Evaluate with each plan item's subject checked too (see
// completeness.PlanSubject), including the subject-file requirement.
func (e *CompletenessEvaluator) Plan(ctx context.Context, sess *session.Session, stored []*session.Obligation, items []session.PlanItem) (*session.Evaluation, error) {
	return e.evaluate(sess, stored, items)
}

// HasManifest reports whether one of completeness.ManifestPaths exists in the
// project, without reading it. A path that can't be checked counts as there,
// as completeness.Load reports it as an invalid manifest rather than none.
func (e *CompletenessEvaluator) HasManifest(projectPath string) bool {
	for _, rel := range completeness.ManifestPaths {
		if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (e *CompletenessEvaluator) evaluate(sess *session.Session, stored []*session.Obligation, items []session.PlanItem) (*session.Evaluation, error) {
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

	ev := &session.Evaluation{Manifest: m.Path, Gate: m.Gate, CommandsEnabled: e.commands}
	rechecks := newRechecks()
	var problems []string
	for i, item := range items {
		ps, err := completeness.PlanSubject(m, item.RuleID, item.Vars)
		if err != nil {
			problems = append(problems, fmt.Sprintf("items[%d]: %v", i, err))
			continue
		}
		ev.Planned = append(ev.Planned, session.PlannedSubject{RuleID: ps.RuleID, Subject: ps.Subject})
		rechecks.add(completeness.Recheck{RuleID: ps.RuleID, Subject: ps.Subject, Declared: true, SubjectFile: true})
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", session.ErrInvalidPlan, strings.Join(problems, "; "))
	}

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

	for _, o := range stored {
		switch {
		case o.Status == session.ObligationVoid:
		case o.Planned:
			rechecks.add(completeness.Recheck{RuleID: o.RuleID, Subject: o.Subject, Declared: true, SubjectFile: true})
		case o.InheritedFrom != "":
			// Any status: a waived or attested one still voids once its
			// file is gone.
			rechecks.add(completeness.Recheck{RuleID: o.RuleID, Subject: o.Subject})
		case ev.Partial && o.Status != session.ObligationWaived:
			rechecks.add(completeness.Recheck{RuleID: o.RuleID, Subject: o.Subject})
		}
	}
	res := completeness.EvaluateWith(sess.ProjectPath, m, overrides, changes, rechecks.list, completeness.Options{Commands: e.commands})
	ev.Warnings = append(ev.Warnings, res.Warnings...)
	for _, d := range res.Dropped {
		ev.Dropped = append(ev.Dropped, session.DroppedSubject{RuleID: d.RuleID, Subject: d.Subject, Reason: d.Reason})
		ev.Warnings = append(ev.Warnings, fmt.Sprintf("rule %s no longer applies to %s: %s", d.RuleID, d.Subject, d.Reason))
	}
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
		ev.Rules = append(ev.Rules, session.RuleInfo{ID: r.ID, Describe: r.Describe, Stack: r.Stack, Plannable: r.Plannable, Requires: r.Requires,
			PlanHints: r.PlanHints, PlanVars: r.PlanVars})
	}
	var tree *string // the project's content hash, taken once per evaluation
	for _, f := range res.Findings {
		var cmd *session.CommandSpec
		if f.Command != nil {
			cmd = &session.CommandSpec{Line: f.Command.Line, Root: f.Command.Root, Env: f.Command.Env}
			cmd.Fingerprint = e.fingerprint(sess.ProjectPath, cmd, &tree, ev)
		}
		ev.Findings = append(ev.Findings, session.CompletenessFinding{
			RuleID:        f.RuleID,
			RequirementID: f.RequirementID,
			Subject:       f.Subject,
			Source:        f.Source,
			Severity:      f.Severity,
			Describe:      f.Describe,
			Expected:      f.Expected,
			Satisfied:     f.Satisfied,
			ChecksPassed:  f.ChecksPassed,
			Levels:        f.Levels,
			AttestPrompt:  f.AttestPrompt,
			Command:       cmd,
		})
	}
	return ev, nil
}

// fingerprint identifies the state spec would run against: the whole
// repository's content (gitctx.WorkTreeHash, taken once per evaluation and
// kept in *tree; a test suite can read code outside its stack root) and its
// env. It is "" when the content can't be hashed, with a warning, so a pass
// counts only in the evaluation that ran it.
func (e *CompletenessEvaluator) fingerprint(projectPath string, spec *session.CommandSpec, tree **string, ev *session.Evaluation) string {
	if *tree == nil {
		h, err := gitctx.WorkTreeHash(projectPath)
		if err != nil {
			ev.Warnings = append(ev.Warnings, fmt.Sprintf("command check results can't be tied to the project's state (%v), so a pass counts only for the run that made it: run tf_verify, or let tf_stop run them", err))
		}
		*tree = &h
	}
	if **tree == "" {
		return ""
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "tree %s\n", **tree)
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(sum, "env %q=%q\n", k, spec.Env[k])
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// rechecks collects one recheck per (rule, subject), merging their flags.
type rechecks struct {
	list  []completeness.Recheck
	index map[string]int
}

func newRechecks() *rechecks {
	return &rechecks{index: map[string]int{}}
}

func (r *rechecks) add(rc completeness.Recheck) {
	k := rc.RuleID + "\x00" + rc.Subject
	if i, ok := r.index[k]; ok {
		r.list[i].Declared = r.list[i].Declared || rc.Declared
		r.list[i].SubjectFile = r.list[i].SubjectFile || rc.SubjectFile
		return
	}
	r.index[k] = len(r.list)
	r.list = append(r.list, rc)
}
