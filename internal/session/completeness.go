package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ObligationStatus is the state of one obligation. Waived is set by tf_waive
// and evaluation never changes it. Attested is set by tf_attest; evaluation
// then moves the obligation between attested (the claim stands alone) and
// satisfied (the claim plus passing checks meet the requirement), never back
// to open.
type ObligationStatus string

const (
	ObligationOpen      ObligationStatus = "open"
	ObligationSatisfied ObligationStatus = "satisfied"
	ObligationAttested  ObligationStatus = "attested"
	ObligationWaived    ObligationStatus = "waived"
	ObligationVoid      ObligationStatus = "void"
)

// levelAttest is the verified level an attestation adds, and levelCommand
// the one a passing command check adds.
const (
	levelAttest  = "attest"
	levelCommand = "command"
)

// Obligation is one requirement applied to one subject in one session.
type Obligation struct {
	ID               string           `json:"id"`
	SessionID        string           `json:"session_id"`
	RuleID           string           `json:"rule_id"`
	RequirementID    string           `json:"requirement_id"`
	Subject          string           `json:"subject"`
	Source           string           `json:"source"`
	Severity         string           `json:"severity"`
	Describe         string           `json:"describe"`
	Expected         []string         `json:"expected"`
	Status           ObligationStatus `json:"status"`
	VerifiedLevels   []string         `json:"verified_levels"`
	Evidence         string           `json:"evidence"`
	WaiverDecisionID string           `json:"waiver_decision_id"`
	Planned          bool             `json:"planned"`
	InheritedFrom    string           `json:"inherited_from"`
	RemindedCount    int              `json:"reminded_count"`
	// CommandCheck is the last run of the obligation's command check. A pass
	// counts only for the same command line, and only while the project is
	// as it was when the command ran (CommandSpec.Fingerprint).
	CommandCheck *CommandCheck `json:"command_check,omitempty"`
	FirstSeenAt  time.Time     `json:"first_seen_at"`
	SatisfiedAt  *time.Time    `json:"satisfied_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

func (o *Obligation) key() string {
	return o.RuleID + "\x00" + o.RequirementID + "\x00" + o.Subject
}

func (o *Obligation) subjectKey() string {
	return subjectKey(o.RuleID, o.Subject)
}

func subjectKey(ruleID, subject string) string {
	return ruleID + "\x00" + subject
}

// attested reports whether the agent has attested the obligation; it keeps
// the attestation when its checks later pass and it becomes satisfied.
func (o *Obligation) attested() bool {
	return o.Status == ObligationAttested || slices.Contains(o.VerifiedLevels, levelAttest)
}

// withAttest returns levels plus the attest level.
func withAttest(levels []string) []string {
	if slices.Contains(levels, levelAttest) {
		return levels
	}
	return append(slices.Clone(levels), levelAttest)
}

// CompletenessFinding is one requirement checked against one subject.
type CompletenessFinding struct {
	RuleID        string
	RequirementID string
	Subject       string
	Source        string
	Severity      string
	Describe      string
	Expected      []string
	// Satisfied means the requirement is met without an attestation.
	Satisfied bool
	// ChecksPassed means its checks passed; with an attestation on record,
	// the requirement is met.
	ChecksPassed bool
	Levels       []string
	// AttestPrompt is set when the requirement needs an attestation.
	AttestPrompt string
	// Command is the requirement's command check, set only while command
	// checks are enabled and one resolved. Satisfied and ChecksPassed leave
	// it out: the requirement is met only once it passes too.
	Command *CommandSpec
	// CommandResult is the command run for this evaluation (tf_verify, or a
	// completing stop); nil when it wasn't run, and the obligation's last run
	// stands.
	CommandResult *CommandCheck
}

func (f *CompletenessFinding) key() string {
	return f.RuleID + "\x00" + f.RequirementID + "\x00" + f.Subject
}

// CommandSpec is a command check from the project's manifest or overrides,
// never from tool arguments.
type CommandSpec struct {
	Line string
	// Root is the directory it runs in, relative to the project.
	Root string
	// Env is merged over the inherited environment.
	Env map[string]string
	// Fingerprint identifies the state the command would run against: the
	// content under Root and Env. "" when it can't be taken (no git), and
	// then a pass counts only in the evaluation that ran it.
	Fingerprint string
}

func (c *CommandSpec) runKey() string { return c.Root + "\x00" + c.Line }

// CommandCheck is one run of a command check: exit status 0 passes.
type CommandCheck struct {
	Command  string `json:"command"`
	Root     string `json:"root"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	// TimedOut means it was killed at the time limit.
	TimedOut bool `json:"timed_out,omitempty"`
	// Error says why it couldn't start, or why it was cut short; ExitCode is
	// then -1.
	Error string `json:"error,omitempty"`
	// Output is the combined stdout and stderr, truncated to its end.
	Output string    `json:"output,omitempty"`
	RanAt  time.Time `json:"ran_at"`
	// Fingerprint is the spec's when it ran.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// runs reports whether c is a run of spec.
func (c *CommandCheck) runs(spec *CommandSpec) bool {
	return c != nil && spec != nil && c.Command == spec.Line && c.Root == spec.Root
}

// current reports whether c, a run of spec, ran against the state spec would
// run against now: nothing under its root or in its env changed since. With
// no fingerprint either way, it can't be told, so it isn't.
func (c *CommandCheck) current(spec *CommandSpec) bool {
	return c.runs(spec) && c.Fingerprint != "" && c.Fingerprint == spec.Fingerprint
}

// result is c's outcome in a few words.
func (c *CommandCheck) result() string {
	switch {
	case c == nil:
		return "not run yet"
	case c.Passed:
		return "passed"
	case c.TimedOut:
		return "timed out"
	case c.Error != "":
		return "failed: " + c.Error
	}
	return fmt.Sprintf("failed (exit %d)", c.ExitCode)
}

// withCommand folds f's command check into its result: with a command,
// f is met (and its checks pass) only once the command passes, on this
// evaluation's run, or else on prev, the obligation's last run, while it is
// current: the same command line against the same project state. A pass
// from before a change counts for nothing, so checks that don't run
// commands (checkpoints, taskflow check) can't pass on stale results. It
// returns the check to keep on the obligation.
func (f *CompletenessFinding) withCommand(prev *CommandCheck) (satisfied, checksPassed bool, levels []string, check *CommandCheck) {
	check = prev
	fresh := f.CommandResult != nil
	if fresh {
		check = f.CommandResult
	}
	if f.Command == nil {
		return f.Satisfied, f.ChecksPassed, f.Levels, check
	}
	passed := check.runs(f.Command) && check.Passed && (fresh || check.current(f.Command))
	levels = f.Levels
	if passed {
		levels = append(slices.Clone(levels), levelCommand)
	}
	return f.Satisfied && passed, f.ChecksPassed && passed, levels, check
}

// StackInfo is a manifest stack with its commands resolved for the project.
type StackInfo struct {
	ID   string `json:"id"`
	Root string `json:"root"`
	// Commands maps each command name to its command line; null when none
	// resolved.
	Commands map[string]*string `json:"commands"`
	Warning  string             `json:"warning,omitempty"`
}

type RuleInfo struct {
	ID        string   `json:"id"`
	Describe  string   `json:"describe"`
	Stack     string   `json:"stack,omitempty"`
	Plannable bool     `json:"plannable,omitempty"`
	Requires  []string `json:"requires"`
	// PlanHints are task keywords that suggest planning the rule.
	PlanHints []string `json:"-"`
	// PlanVars are the vars a tf_plan item for the rule takes.
	PlanVars []string `json:"-"`
}

// PlanItem is one subject the agent expects to create, declared with
// tf_plan: a plannable rule and the vars that name its file.
type PlanItem struct {
	RuleID string            `json:"rule_id"`
	Vars   map[string]string `json:"vars"`
}

// PlannedSubject is a plan item resolved against the manifest.
type PlannedSubject struct {
	RuleID  string
	Subject string
}

// DroppedSubject is a stored obligation's rule and subject that the manifest
// no longer applies to.
type DroppedSubject struct {
	RuleID  string
	Subject string
	// Reason says why (the rule left the manifest, its stack is missing, or
	// the subject no longer fits the rule).
	Reason string
}

// Evaluation is the result of checking a session's changes against the
// project's completeness manifest.
type Evaluation struct {
	// Manifest is the manifest's path relative to the project.
	Manifest string
	// Error is set when the manifest exists but is invalid; the evaluation
	// then has no findings and stored obligations are left as they are.
	Error    string
	Stacks   []StackInfo
	Rules    []RuleInfo
	Findings []CompletenessFinding
	// Partial means the changed set may be incomplete (no git, only
	// uncommitted changes readable, or capped), so stored obligations that
	// weren't found are kept rather than voided. Dropped ones are voided
	// regardless.
	Partial  bool
	Warnings []string
	// Gate is the manifest's completion gate, "" when it sets none.
	Gate string
	// Planned are the subjects of the plan items passed to Plan.
	Planned []PlannedSubject
	// CommandsEnabled means command checks are on (completeness.allow_commands
	// in config): findings carry the ones that resolved, for tf_verify and a
	// completing stop to run.
	CommandsEnabled bool
	// Dropped are rechecked (rule, subject) pairs the manifest can no longer
	// apply to: the rule was removed or renamed, its stack is missing, or the
	// subject is outside the stack root or no longer fits the rule. Their
	// obligations, planned and inherited ones included, are voided whatever
	// their status, even in a partial evaluation: dropping is decided by the
	// manifest, not by the change set.
	Dropped []DroppedSubject
}

// ErrInvalidPlan is returned by Plan for items the manifest can't plan.
var ErrInvalidPlan = errors.New("invalid plan")

// CompletenessEvaluator checks a session's project against its completeness
// manifest. A nil evaluator turns completeness off.
type CompletenessEvaluator interface {
	// BaseSHA returns the commit a new session's changes are measured from.
	BaseSHA(projectPath string) (string, error)
	// Evaluate returns nil, nil when the project has no manifest. stored are
	// the session's obligations: planned and inherited ones are verified
	// again even if their subject isn't in the changed set, and when it is
	// partial, open, satisfied, and attested ones too. Those the manifest can
	// no longer apply to are reported in Evaluation.Dropped.
	Evaluate(ctx context.Context, sess *Session, stored []*Obligation) (*Evaluation, error)
	// Plan is Evaluate with the plan items' subjects checked too, whether or
	// not their files exist yet; their findings include the subject-file
	// requirement. Items the manifest can't plan return an error wrapping
	// ErrInvalidPlan. Like Evaluate, it returns nil, nil with no manifest.
	Plan(ctx context.Context, sess *Session, stored []*Obligation, items []PlanItem) (*Evaluation, error)
	// HasManifest reports whether the project has a manifest, valid or not,
	// without evaluating anything: whether completeness is on for callers
	// that don't evaluate (Waive).
	HasManifest(projectPath string) bool
	// RunCommands runs command checks in the session's project, in order,
	// one result per spec. A command that fails, times out, or can't start
	// is a failed check, never an error, so it can't fail a stop.
	RunCommands(ctx context.Context, sess *Session, specs []CommandSpec) []CommandCheck
}

// CompletenessSummary counts obligations by status (void ones aren't counted).
type CompletenessSummary struct {
	Open      int `json:"open"`
	Satisfied int `json:"satisfied"`
	Attested  int `json:"attested"`
	Waived    int `json:"waived"`
}

// ObligationView is an obligation as shown to the agent.
type ObligationView struct {
	ObligationID string           `json:"obligation_id"`
	Describe     string           `json:"describe"`
	Expected     string           `json:"expected,omitempty"`
	Severity     string           `json:"severity"`
	Rule         string           `json:"rule"`
	Requirement  string           `json:"requirement"`
	Subject      string           `json:"subject"`
	Status       ObligationStatus `json:"status,omitempty"`
	// Attest is what the agent will be asked to attest, when the
	// requirement needs an attestation.
	Attest string `json:"attest,omitempty"`
	// Evidence is the attestation, or a waiver's rationale.
	Evidence string `json:"evidence,omitempty"`
	// Command is the obligation's command check, when one applies.
	Command *CommandView `json:"command,omitempty"`
	// FromSession is the session an inherited obligation was copied from.
	FromSession string `json:"from_session,omitempty"`
}

// CommandView is an obligation's command check and how its last run went.
type CommandView struct {
	Command string `json:"command"`
	Root    string `json:"root"`
	// Result is "not run yet", "passed", "failed (exit N)", "timed out",
	// "failed: …" (it couldn't start, or was killed), or "stale: …" (it
	// passed, but the project changed since).
	Result string `json:"result"`
	// Output is the end of a failed run's output.
	Output string `json:"output,omitempty"`
}

// CheckpointCompleteness is what changed since the agent last heard about
// completeness, returned with each checkpoint.
type CheckpointCompleteness struct {
	Summary        CompletenessSummary `json:"summary"`
	NewlyOpen      []ObligationView    `json:"newly_open"`
	NewlySatisfied []string            `json:"newly_satisfied"`
	StillOpen      []string            `json:"still_open"`
	// PlanReminder repeats until the agent plans or declines a suggested
	// plan.
	PlanReminder string   `json:"plan_reminder,omitempty"`
	Error        string   `json:"error,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// ContractCompleteness is the start contract's completeness section: the
// definition of done for the project, what the session owes, and how to
// resolve it.
type ContractCompleteness struct {
	Manifest string              `json:"manifest"`
	Error    string              `json:"error,omitempty"`
	Gate     GateMode            `json:"gate"`
	Stacks   []StackInfo         `json:"stacks"`
	Rules    []RuleInfo          `json:"rules"`
	Summary  CompletenessSummary `json:"summary"`
	Open     []ObligationView    `json:"open"`
	// Inherited are the obligations a new session took over from the task's
	// finished one.
	Inherited     []ObligationView `json:"inherited,omitempty"`
	SuggestedPlan *SuggestedPlan   `json:"suggested_plan,omitempty"`
	Protocol      string           `json:"protocol"`
	// CommandsEnabled means command checks run at stop and with tf_verify.
	CommandsEnabled bool     `json:"commands_enabled,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// CompletenessReport is the full report for tf_completeness.
type CompletenessReport struct {
	SessionID   string              `json:"session_id"`
	Enabled     bool                `json:"enabled"`
	Manifest    string              `json:"manifest,omitempty"`
	Error       string              `json:"error,omitempty"`
	Summary     CompletenessSummary `json:"summary"`
	Obligations []ObligationView    `json:"obligations"`
	Stacks      []StackInfo         `json:"stacks,omitempty"`
	Rules       []RuleInfo          `json:"rules,omitempty"`
	// CommandsEnabled means command checks run at stop and with tf_verify.
	CommandsEnabled bool `json:"commands_enabled,omitempty"`
	// Commands are the command checks this call ran (tf_verify).
	Commands []CommandCheck `json:"commands,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

// mergeResult is an evaluation merged into a session's stored obligations.
type mergeResult struct {
	obligations    []*Obligation // every obligation of the session, after the merge
	upserts        []*Obligation // new or changed, to write
	voided         []*Obligation // to mark void
	newlyOpen      map[string]bool
	newlySatisfied []*Obligation
	attest         map[string]string       // obligation ID → attest prompt
	commands       map[string]*CommandSpec // obligation ID → command check
}

// mergeObligations folds an evaluation's findings into the stored
// obligations:
//   - a new key is inserted as open or satisfied, and as planned when its
//     subject was planned;
//   - waived obligations are left alone (sticky);
//   - attested ones are recomputed attested ⇄ satisfied: the attestation
//     stands, and passing checks complete it;
//   - other existing ones are recomputed open ⇄ satisfied (a void one that
//     is found again comes back);
//   - stored keys that weren't found are voided (e.g. the file was reverted),
//     unless they were planned, or the evaluation is partial, or they
//     originated in this session and were waived or attested. An inherited
//     obligation that isn't found is voided whatever its status: it is
//     rechecked only while its file exists, so the work it named is gone;
//   - stored keys whose (rule, subject) the manifest dropped (ev.Dropped)
//     are voided whatever their status, planned or not, even when the
//     evaluation is partial, since the manifest decides that, not the
//     change set.
func mergeObligations(sessionID string, stored []*Obligation, ev *Evaluation, now time.Time) *mergeResult {
	mr := &mergeResult{newlyOpen: map[string]bool{}, attest: map[string]string{}, commands: map[string]*CommandSpec{}}
	byKey := make(map[string]*Obligation, len(stored))
	dropped := make(map[string]bool, len(ev.Dropped))
	for _, d := range ev.Dropped {
		dropped[subjectKey(d.RuleID, d.Subject)] = true
	}
	planned := map[string]bool{}
	for _, o := range stored {
		byKey[o.key()] = o
		mr.obligations = append(mr.obligations, o)
		if o.Planned {
			planned[o.subjectKey()] = true
		}
	}
	for _, p := range ev.Planned {
		planned[subjectKey(p.RuleID, p.Subject)] = true
	}

	found := map[string]bool{}
	for i := range ev.Findings {
		f := &ev.Findings[i]
		k := f.key()
		if found[k] {
			continue
		}
		found[k] = true
		o, ok := byKey[k]
		var prevCheck *CommandCheck
		if ok {
			prevCheck = o.CommandCheck
		}
		satisfied, checksPassed, levels, check := f.withCommand(prevCheck)
		status := ObligationOpen
		if satisfied {
			status = ObligationSatisfied
		}

		if !ok {
			o = &Obligation{
				ID:             uuid.New().String(),
				SessionID:      sessionID,
				RuleID:         f.RuleID,
				RequirementID:  f.RequirementID,
				Subject:        f.Subject,
				Source:         f.Source,
				Severity:       f.Severity,
				Describe:       f.Describe,
				Expected:       f.Expected,
				Status:         status,
				VerifiedLevels: levels,
				CommandCheck:   check,
				Planned:        planned[subjectKey(f.RuleID, f.Subject)],
				FirstSeenAt:    now,
				UpdatedAt:      now,
			}
			if status == ObligationSatisfied {
				o.SatisfiedAt = &now
			} else {
				mr.newlyOpen[o.ID] = true
			}
			mr.obligations = append(mr.obligations, o)
			mr.upserts = append(mr.upserts, o)
			if f.AttestPrompt != "" {
				mr.attest[o.ID] = f.AttestPrompt
			}
			if f.Command != nil {
				mr.commands[o.ID] = f.Command
			}
			continue
		}
		if f.AttestPrompt != "" {
			mr.attest[o.ID] = f.AttestPrompt
		}
		if f.Command != nil {
			mr.commands[o.ID] = f.Command
		}

		changed := o.Source != f.Source || o.Severity != f.Severity || o.Describe != f.Describe || !slices.Equal(o.Expected, f.Expected)
		o.Source, o.Severity, o.Describe, o.Expected = f.Source, f.Severity, f.Describe, f.Expected
		if f.CommandResult != nil && o.Status != ObligationWaived {
			o.CommandCheck = f.CommandResult
			changed = true
		}
		switch {
		case o.Status == ObligationWaived:
		case o.attested():
			levels, next := withAttest(levels), ObligationAttested
			if checksPassed {
				next = ObligationSatisfied
			}
			if !slices.Equal(o.VerifiedLevels, levels) {
				o.VerifiedLevels = levels
				changed = true
			}
			if o.Status != next {
				o.Status = next
				changed = true
				if next == ObligationSatisfied {
					o.SatisfiedAt = &now
				} else {
					o.SatisfiedAt = nil
				}
			}
		default:
			prev := o.Status
			if !slices.Equal(o.VerifiedLevels, levels) {
				o.VerifiedLevels = levels
				changed = true
			}
			if prev != status {
				o.Status = status
				changed = true
				if status == ObligationSatisfied {
					o.SatisfiedAt = &now
					if prev == ObligationOpen {
						mr.newlySatisfied = append(mr.newlySatisfied, o)
					}
				} else {
					o.SatisfiedAt = nil
					mr.newlyOpen[o.ID] = true
				}
			}
		}
		if changed {
			o.UpdatedAt = now
			mr.upserts = append(mr.upserts, o)
		}
	}

	for _, o := range stored {
		if o.Status == ObligationVoid || found[o.key()] {
			continue
		}
		if !dropped[o.subjectKey()] {
			if ev.Partial || o.Planned {
				continue
			}
			if o.InheritedFrom == "" && (o.Status == ObligationWaived || o.attested()) {
				continue
			}
		}
		o.Status = ObligationVoid
		o.UpdatedAt = now
		mr.voided = append(mr.voided, o)
	}
	return mr
}

// completenessState is one evaluation and the session's obligations after it.
type completenessState struct {
	eval           *Evaluation
	obligations    []*Obligation
	newlyOpen      map[string]bool
	newlySatisfied []*Obligation
	attest         map[string]string
	commands       map[string]*CommandSpec // obligation ID → command check
	// runs are the command checks run for this evaluation.
	runs []CommandCheck
	// readErr is set when the stored obligations couldn't be read; the state
	// then can't say what is owed, and must not claim nothing is.
	readErr string
	// unsaved are the IDs of obligations merged in memory but not written
	// yet (see mergeEvaluation); they aren't known to the store.
	unsaved map[string]bool
}

// evaluateCompleteness evaluates sess and, when merge is set and the
// manifest is valid, writes the result to its obligations. It returns nil
// when completeness is off for the session (no evaluator or no manifest).
// Failures are logged and never fail the caller.
func (s *Service) evaluateCompleteness(ctx context.Context, sess *Session, merge bool) *completenessState {
	st, err := s.tryEvaluateCompleteness(ctx, sess, merge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[session] completeness evaluation failed for session %s: %v\n", sess.ID, err)
		return nil
	}
	return st
}

// tryEvaluateCompleteness is evaluateCompleteness, returning the evaluator's
// error rather than logging it, for callers that must not treat a failed
// evaluation as "completeness is off" (the completion gate).
func (s *Service) tryEvaluateCompleteness(ctx context.Context, sess *Session, merge bool) (*completenessState, error) {
	if s.completeness == nil {
		return nil, nil
	}
	stored, listErr := s.store.ListObligations(ctx, sess.ID)
	if listErr != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to list obligations for session %s: %v\n", sess.ID, listErr)
	}
	ev, err := s.completeness.Evaluate(ctx, sess, stored)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, nil
	}
	if listErr != nil {
		return &completenessState{eval: ev, readErr: "stored obligations couldn't be read: " + listErr.Error()}, nil
	}
	if !merge {
		return &completenessState{eval: ev, obligations: stored, newlyOpen: map[string]bool{}, attest: map[string]string{}, commands: map[string]*CommandSpec{}}, nil
	}
	return s.applyEvaluation(ctx, sess, stored, ev), nil
}

// applyEvaluation merges ev into the session's stored obligations and writes
// the result, unless the manifest is invalid. Failed writes are logged and
// never fail the caller; callers that must know use mergeEvaluation and
// persistMerge.
func (s *Service) applyEvaluation(ctx context.Context, sess *Session, stored []*Obligation, ev *Evaluation) *completenessState {
	st, mr := mergeEvaluation(sess, stored, ev)
	_ = s.persistMerge(ctx, sess, st, mr)
	return st
}

// mergeEvaluation merges ev into the session's stored obligations in memory,
// writing nothing. The merge to write is nil when the manifest is invalid
// (stored obligations are then left as they are); persistMerge writes it.
// Until then, obligations the merge inserted are marked unsaved in the state.
func mergeEvaluation(sess *Session, stored []*Obligation, ev *Evaluation) (*completenessState, *mergeResult) {
	st := &completenessState{eval: ev, obligations: stored, newlyOpen: map[string]bool{}, attest: map[string]string{}, commands: map[string]*CommandSpec{}}
	if ev.Error != "" {
		return st, nil
	}
	have := make(map[string]bool, len(stored))
	for _, o := range stored {
		have[o.ID] = true
	}
	mr := mergeObligations(sess.ID, stored, ev, time.Now())
	st.obligations = mr.obligations
	st.newlyOpen = mr.newlyOpen
	st.newlySatisfied = mr.newlySatisfied
	st.attest = mr.attest
	st.commands = mr.commands
	st.unsaved = map[string]bool{}
	for _, o := range mr.upserts {
		if !have[o.ID] {
			st.unsaved[o.ID] = true
		}
	}
	return st, mr
}

// persistMerge writes a merge from mergeEvaluation (writeMerge) and, when
// every write succeeds, queues the session's checklist for ClickUp unless
// the task's latest checklist op already carries it (enqueueChecklist), so
// a checklist that drifted is repaired even by a merge that changed
// nothing. After a failure it isn't queued: the stored obligations still
// differ from the merge, so the next evaluation finds the change again.
func (s *Service) persistMerge(ctx context.Context, sess *Session, st *completenessState, mr *mergeResult) error {
	err := s.writeMerge(ctx, st, mr)
	if mr != nil && err == nil {
		s.enqueueChecklist(ctx, sess, st.obligations)
	}
	return err
}

// writeMerge writes a merge from mergeEvaluation: its upserts, then its
// voids. A failed write is logged and the others still go ahead; the
// returned error joins every failure, nil when all were written (or there
// is no merge: the manifest is invalid).
func (s *Service) writeMerge(ctx context.Context, st *completenessState, mr *mergeResult) error {
	if mr == nil {
		return nil
	}
	var errs []error
	for _, o := range mr.upserts {
		id := o.ID
		if err := s.store.UpsertObligation(ctx, o); err != nil {
			fmt.Fprintf(os.Stderr, "[session] failed to save obligation %s/%s for %s: %v\n", o.RuleID, o.RequirementID, o.Subject, err)
			errs = append(errs, fmt.Errorf("obligation %s/%s for %s: %w", o.RuleID, o.RequirementID, o.Subject, err))
			continue
		}
		// Another evaluation (the MCP server and the CLI) inserted the key
		// first; the upsert kept its ID, so follow it.
		if o.ID != id {
			if st.newlyOpen[id] {
				delete(st.newlyOpen, id)
				st.newlyOpen[o.ID] = true
			}
			if prompt, ok := st.attest[id]; ok {
				delete(st.attest, id)
				st.attest[o.ID] = prompt
			}
			if spec, ok := st.commands[id]; ok {
				delete(st.commands, id)
				st.commands[o.ID] = spec
			}
		}
	}
	for _, o := range mr.voided {
		if err := s.store.UpdateObligationStatus(ctx, o.ID, ObligationVoid, o.UpdatedAt); err != nil {
			fmt.Fprintf(os.Stderr, "[session] failed to void obligation %s: %v\n", o.ID, err)
			errs = append(errs, fmt.Errorf("voiding obligation %s: %w", o.ID, err))
		}
	}
	st.unsaved = nil
	return errors.Join(errs...)
}

// evaluateUnsaved is tryEvaluateCompleteness merging in memory only: it
// returns the merge for persistMerge to write once the caller decides to go
// ahead (Stop, after the completion gate). The merge is nil when there is
// nothing to write. With run set and command checks enabled, the findings'
// command checks are run first (runCommandChecks), so the merge has their
// results.
func (s *Service) evaluateUnsaved(ctx context.Context, sess *Session, run bool) (*completenessState, *mergeResult, error) {
	st, err := s.tryEvaluateCompleteness(ctx, sess, false)
	if st == nil || err != nil || st.readErr != "" {
		return st, nil, err
	}
	var runs []CommandCheck
	if run && st.eval.Error == "" && st.eval.CommandsEnabled {
		runs = s.runCommandChecks(ctx, sess, st.eval, st.obligations)
	}
	merged, mr := mergeEvaluation(sess, st.obligations, st.eval)
	merged.runs = runs
	return merged, mr, nil
}

// runCommandChecks runs the command checks of ev's findings, once per
// distinct command line and root, and attaches each result to its findings.
// Waived obligations' checks aren't run. It returns the runs.
func (s *Service) runCommandChecks(ctx context.Context, sess *Session, ev *Evaluation, stored []*Obligation) []CommandCheck {
	waived := map[string]bool{}
	for _, o := range stored {
		if o.Status == ObligationWaived {
			waived[o.key()] = true
		}
	}
	var specs []CommandSpec
	index := map[string]int{}
	for i := range ev.Findings {
		f := &ev.Findings[i]
		if f.Command == nil || waived[f.key()] {
			continue
		}
		if _, ok := index[f.Command.runKey()]; !ok {
			index[f.Command.runKey()] = len(specs)
			specs = append(specs, *f.Command)
		}
	}
	if len(specs) == 0 {
		return nil
	}
	runs := s.completeness.RunCommands(ctx, sess, specs)
	if len(runs) != len(specs) {
		// A broken evaluator mustn't pass anything: each spec fails.
		runs = make([]CommandCheck, len(specs))
		for i, spec := range specs {
			runs[i] = CommandCheck{Command: spec.Line, Root: spec.Root, ExitCode: -1, Error: "the command runner returned no result", RanAt: time.Now()}
		}
	}
	// The state each run is tied to is the spec's, taken before it ran: a
	// change made while it ran makes it stale too.
	for i := range runs {
		runs[i].Fingerprint = specs[i].Fingerprint
	}
	for i := range ev.Findings {
		f := &ev.Findings[i]
		if f.Command == nil || waived[f.key()] {
			continue
		}
		run := runs[index[f.Command.runKey()]]
		f.CommandResult = &run
	}
	return runs
}

// errText is the evaluation's manifest error, or why the obligations
// couldn't be read.
func (st *completenessState) errText() string {
	if st.readErr != "" {
		return st.readErr
	}
	return st.eval.Error
}

func (st *completenessState) summary() CompletenessSummary {
	var sum CompletenessSummary
	for _, o := range st.obligations {
		switch o.Status {
		case ObligationOpen:
			sum.Open++
		case ObligationSatisfied:
			sum.Satisfied++
		case ObligationAttested:
			sum.Attested++
		case ObligationWaived:
			sum.Waived++
		}
	}
	return sum
}

func (st *completenessState) view(o *Obligation, withStatus bool) ObligationView {
	v := ObligationView{
		ObligationID: o.ID,
		Describe:     o.Describe,
		Expected:     strings.Join(o.Expected, ", "),
		Severity:     o.Severity,
		Rule:         o.RuleID,
		Requirement:  o.RequirementID,
		Subject:      o.Subject,
		Attest:       st.attest[o.ID],
	}
	if spec := st.commands[o.ID]; spec != nil {
		check := o.CommandCheck
		if !check.runs(spec) {
			check = nil
		}
		v.Command = &CommandView{Command: spec.Line, Root: spec.Root, Result: check.result()}
		switch {
		case check == nil:
		case !check.Passed:
			v.Command.Output = check.Output
		case !check.current(spec) && !st.ranNow(check):
			v.Command.Result = "stale: it passed, but the project changed since; run tf_verify again"
		}
	}
	if withStatus {
		v.Status = o.Status
		v.Evidence = o.Evidence
	}
	return v
}

func (st *completenessState) views(obligations []*Obligation, withStatus bool) []ObligationView {
	out := []ObligationView{}
	for _, o := range obligations {
		out = append(out, st.view(o, withStatus))
	}
	return out
}

// find returns the session's obligation with id, or nil.
func (st *completenessState) find(id string) *Obligation {
	for _, o := range st.obligations {
		if o.ID == id {
			return o
		}
	}
	return nil
}

// open returns the open obligations, required ones first.
func (st *completenessState) open() []*Obligation {
	var out []*Obligation
	for _, o := range st.obligations {
		if o.Status == ObligationOpen {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Severity == "required" && out[j].Severity != "required"
	})
	return out
}

// openRequired returns the open obligations that hold up completion.
func (st *completenessState) openRequired() []*Obligation {
	var out []*Obligation
	for _, o := range st.open() {
		if o.Severity == "required" {
			out = append(out, o)
		}
	}
	return out
}

// remind counts one reminder for every open obligation: they are about to
// be returned to the agent.
func (s *Service) remind(ctx context.Context, st *completenessState) {
	var ids []string
	for _, o := range st.open() {
		ids = append(ids, o.ID)
		o.RemindedCount++
	}
	if len(ids) == 0 {
		return
	}
	if err := s.store.IncrementObligationReminders(ctx, ids); err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to count obligation reminders: %v\n", err)
	}
}

// checkpointView reports what changed since the agent was last told: open
// obligations it hasn't seen yet (or that reopened) in full, the rest by
// description. Call it before remind.
func (st *completenessState) checkpointView() *CheckpointCompleteness {
	cc := &CheckpointCompleteness{
		Summary:        st.summary(),
		NewlyOpen:      []ObligationView{},
		NewlySatisfied: []string{},
		StillOpen:      []string{},
		Error:          st.errText(),
		Warnings:       st.eval.Warnings,
	}
	for _, o := range st.open() {
		if st.newlyOpen[o.ID] || o.RemindedCount == 0 {
			cc.NewlyOpen = append(cc.NewlyOpen, st.view(o, false))
		} else {
			cc.StillOpen = append(cc.StillOpen, o.Describe)
		}
	}
	for _, o := range st.newlySatisfied {
		cc.NewlySatisfied = append(cc.NewlySatisfied, o.Describe)
	}
	return cc
}

// contractView is the start contract's completeness section. Call it before
// remind.
func (st *completenessState) contractView(gate GateMode) *ContractCompleteness {
	stacks := st.eval.Stacks
	if stacks == nil {
		stacks = []StackInfo{}
	}
	rules := st.eval.Rules
	if rules == nil {
		rules = []RuleInfo{}
	}
	protocol := completenessProtocol
	if st.eval.CommandsEnabled {
		protocol += " " + commandsProtocol
	}
	return &ContractCompleteness{
		Manifest:        st.eval.Manifest,
		Error:           st.errText(),
		Gate:            gate,
		Stacks:          stacks,
		Rules:           rules,
		Summary:         st.summary(),
		Open:            st.views(st.open(), false),
		Protocol:        protocol,
		CommandsEnabled: st.eval.CommandsEnabled,
		Warnings:        st.eval.Warnings,
	}
}

// completenessProtocol tells the agent how obligations are checked and
// resolved.
const completenessProtocol = "Taskflow checks these at every tf_checkpoint and before tf_stop. " +
	"Do the work, or call tf_attest with evidence when it can't be checked from files (e.g. which tests cover it). " +
	"If a requirement genuinely does not apply, call tf_waive with a rationale; it will be posted to the ClickUp task as a decision."

// commandsProtocol is added while command checks are enabled.
const commandsProtocol = "Command checks (an obligation's `command`, e.g. the test suite) run when you complete the session with tf_stop; call tf_verify to run them sooner. A failing command keeps its obligation open."

var statusRank = map[ObligationStatus]int{ObligationOpen: 0, ObligationAttested: 1, ObligationWaived: 2, ObligationSatisfied: 3}

func (st *completenessState) report(sessionID string) *CompletenessReport {
	r := &CompletenessReport{
		SessionID:   sessionID,
		Enabled:     true,
		Manifest:    st.eval.Manifest,
		Error:       st.errText(),
		Summary:     st.summary(),
		Obligations: []ObligationView{},
		Stacks:      st.eval.Stacks,
		Rules:       st.eval.Rules,
		Warnings:    st.eval.Warnings,

		CommandsEnabled: st.eval.CommandsEnabled,
		Commands:        st.runs,
	}
	for _, o := range st.visible() {
		r.Obligations = append(r.Obligations, st.view(o, true))
	}
	return r
}

// ranNow reports whether c is one of this evaluation's runs, which count
// whether or not they have a fingerprint.
func (st *completenessState) ranNow(c *CommandCheck) bool {
	for i := range st.runs {
		if r := &st.runs[i]; r.Command == c.Command && r.Root == c.Root && r.RanAt.Equal(c.RanAt) {
			return true
		}
	}
	return false
}

// visible returns the non-void obligations, open first.
func (st *completenessState) visible() []*Obligation {
	var out []*Obligation
	for _, o := range st.obligations {
		if o.Status != ObligationVoid {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return statusRank[out[i].Status] < statusRank[out[j].Status] })
	return out
}

// commentLine is the one-line completeness summary for the checkpoint
// comment, e.g. "**Completeness:** 2/5 · open: story, e2e ×2".
func (st *completenessState) commentLine() string {
	if st.readErr != "" {
		return "**Completeness:** unavailable: " + st.readErr
	}
	if st.eval.Error != "" {
		return "**Completeness:** manifest error: " + st.eval.Error
	}
	sum := st.summary()
	total := sum.Open + sum.Satisfied + sum.Attested + sum.Waived
	if total == 0 {
		return "**Completeness:** nothing owed yet"
	}
	line := fmt.Sprintf("**Completeness:** %d/%d", total-sum.Open, total)
	if sum.Open == 0 {
		return line + " · nothing open"
	}
	counts := map[string]int{}
	var order []string
	for _, o := range st.open() {
		if counts[o.RequirementID] == 0 {
			order = append(order, o.RequirementID)
		}
		counts[o.RequirementID]++
	}
	var parts []string
	for _, id := range order {
		if counts[id] > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", id, counts[id]))
		} else {
			parts = append(parts, id)
		}
	}
	return line + " · open: " + strings.Join(parts, ", ")
}

// reportTable is the completeness section of the "Session Finished" comment.
func (st *completenessState) reportTable() string {
	if st.readErr != "" {
		return "### Completeness\nUnavailable: " + st.readErr
	}
	if st.eval.Error != "" {
		return "### Completeness\nManifest error: " + st.eval.Error
	}
	rows := st.visible()
	sum := st.summary()
	total := sum.Open + sum.Satisfied + sum.Attested + sum.Waived
	var b strings.Builder
	fmt.Fprintf(&b, "### Completeness (%d/%d)\n", total-sum.Open, total)
	if len(rows) == 0 {
		b.WriteString("Nothing owed for this session's changes.")
		return b.String()
	}
	b.WriteString("| Requirement | Subject | Severity | Status | Verified | Evidence |\n|---|---|---|---|---|---|\n")
	for _, o := range rows {
		verified := "—"
		if len(o.VerifiedLevels) > 0 {
			verified = strings.Join(o.VerifiedLevels, ", ") + " ✓"
		}
		evidence := "—"
		if o.Evidence != "" {
			evidence = o.Evidence
		} else if c, spec := o.CommandCheck, st.commands[o.ID]; c != nil && spec != nil {
			result := c.result()
			if c.Passed && !c.current(spec) && !st.ranNow(c) {
				result = "stale (passed before the latest changes)"
			}
			evidence = fmt.Sprintf("`%s`: %s", c.Command, result)
			if !c.Passed {
				if line := lastLine(c.Output); line != "" {
					evidence += ": " + line
				}
			}
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s | %s |\n",
			cell(o.Describe), cell(o.Subject), o.Severity, o.Status, verified, cell(evidence))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// lastLine is the last non-blank line of s, where test runners print their
// summary.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// cell keeps text inside one markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.Join(strings.Fields(s), " ")
}

// Completeness re-evaluates an active session and returns the full report.
// For a session that isn't active, obligations are reported as stored,
// without evaluating: the project may have moved on or lost its manifest.
func (s *Service) Completeness(ctx context.Context, sessionID string) (*CompletenessReport, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session: %w", ErrSessionNotFound)
	}
	if sess.Status != StatusActive {
		return s.storedCompleteness(ctx, sess)
	}
	st := s.evaluateCompleteness(ctx, sess, true)
	if st == nil {
		return &CompletenessReport{SessionID: sessionID, Obligations: []ObligationView{}}, nil
	}
	r := st.report(sessionID)
	s.remind(ctx, st)
	return r, nil
}

// storedCompleteness reports a session's obligations as they were recorded;
// it is enabled when any were.
func (s *Service) storedCompleteness(ctx context.Context, sess *Session) (*CompletenessReport, error) {
	stored, err := s.store.ListObligations(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list obligations: %w", err)
	}
	if len(stored) == 0 {
		return &CompletenessReport{SessionID: sess.ID, Obligations: []ObligationView{}}, nil
	}
	st := &completenessState{eval: &Evaluation{}, obligations: stored}
	r := st.report(sess.ID)
	r.Warnings = []string{fmt.Sprintf("session is %s: obligations are as recorded, not re-evaluated", strings.ToLower(string(sess.Status)))}
	return r, nil
}
