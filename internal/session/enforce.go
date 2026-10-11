package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
)

// GateMode is what Stop(completed) does while required obligations are
// open.
type GateMode string

const (
	// GateReport completes as usual; the stop comment lists what's open.
	GateReport GateMode = "report"
	// GateReview completes locally but moves the ClickUp task to review
	// instead of complete.
	GateReview GateMode = "review"
	// GateBlock refuses to complete with ErrIncompleteSession.
	GateBlock GateMode = "block"
)

// DefaultGate applies when config sets no gate.
const DefaultGate = GateReview

// gateRank orders gates by strictness.
var gateRank = map[GateMode]int{GateReport: 0, GateReview: 1, GateBlock: 2}

// failClosed maps a gate the code doesn't know (a value that got past
// ParseGateMode) to GateBlock, so a bad value can only tighten the gate,
// never loosen it. "" (no gate set) is kept as is.
func (g GateMode) failClosed() GateMode {
	if _, ok := gateRank[g]; ok || g == "" {
		return g
	}
	return GateBlock
}

// stricter returns the stricter of g and other. "" (no gate set) defers to
// the other gate; an unknown gate counts as GateBlock.
func (g GateMode) stricter(other GateMode) GateMode {
	a, b := g.failClosed(), other.failClosed()
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case gateRank[a] > gateRank[b]:
		return a
	}
	return b
}

// ParseGateMode validates a gate name; "" is the default gate. It is the one
// place a gate name from config is checked.
func ParseGateMode(s string) (GateMode, error) {
	g := GateMode(s)
	if g == "" {
		return DefaultGate, nil
	}
	if _, ok := gateRank[g]; ok {
		return g, nil
	}
	return "", fmt.Errorf("invalid completion gate %q: must be report, review, or block", s)
}

// GateOutcome is what the gate did at stop.
type GateOutcome string

const (
	// GatePassed means no required obligation was open.
	GatePassed GateOutcome = "passed"
	// GateReported means the session completed with required work open,
	// listed in the stop comment.
	GateReported GateOutcome = "reported"
	// GateMovedToReview means the session completed with required work open
	// and the ClickUp task goes to review instead of complete.
	GateMovedToReview GateOutcome = "review"
	// GateForced means a human completed a blocked session with --force.
	GateForced GateOutcome = "forced"
)

// GateResult is the completion gate's decision at Stop(completed).
type GateResult struct {
	Mode    GateMode    `json:"mode"`
	Outcome GateOutcome `json:"outcome"`
	// OpenRequired are the required obligations open at stop.
	OpenRequired []ObligationView `json:"open_required,omitempty"`
	// DecisionID is the decision recording a forced completion.
	DecisionID string `json:"decision_id,omitempty"`
	// Unchecked says why completeness couldn't be checked, when the gate
	// acted without knowing what was open.
	Unchecked string `json:"unchecked,omitempty"`
}

// StopResult is a stopped session with its final completeness report and,
// when it was completed, the gate's decision.
type StopResult struct {
	SessionID    string              `json:"session_id"`
	Status       Status              `json:"new_status"`
	Gate         *GateResult         `json:"gate,omitempty"`
	Completeness *CompletenessReport `json:"completeness,omitempty"`
}

// ErrIncompleteSession is returned by Stop(completed) under the block gate
// while required obligations are open; nothing is written, so the session,
// its obligations, and their reminder counts are left unchanged.
var ErrIncompleteSession = errors.New("session has open required obligations")

// IncompleteSessionError lists the obligations holding up completion.
type IncompleteSessionError struct {
	SessionID string
	Open      []ObligationView
	// Unrecorded are the IDs in Open first found by the refused stop: it
	// wrote nothing, so they can't be waived or attested until a checkpoint
	// or tf_completeness records them.
	Unrecorded []string
}

func (e *IncompleteSessionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot complete session %s: %d required obligation(s) open (completion gate: block):", e.SessionID, len(e.Open))
	for _, o := range e.Open {
		fmt.Fprintf(&b, "\n  - %s: %s", o.ObligationID, o.Describe)
		if o.Expected != "" {
			fmt.Fprintf(&b, " (expected: %s)", o.Expected)
		}
		if slices.Contains(e.Unrecorded, o.ObligationID) {
			b.WriteString(" [not recorded yet]")
		}
	}
	b.WriteString("\nDo the work, attest it with tf_attest if it has an attest prompt, or waive it with tf_waive and a rationale, then stop again.")
	if len(e.Unrecorded) > 0 {
		b.WriteString(" Obligations not recorded yet were first found by this stop: call tf_checkpoint or tf_completeness to record them before attesting or waiving.")
	}
	return b.String()
}

func (e *IncompleteSessionError) Unwrap() error { return ErrIncompleteSession }

// ErrCompletenessUnchecked is returned by Stop(completed) under the block
// gate when the session's obligations can't be checked (the manifest is
// invalid, they can't be read, or the evaluation failed). The gate fails
// closed: the session is left unchanged, since nothing shows the required
// work is done. Attest wraps it too, refusing an attestation it can't check
// against the requirement.
var ErrCompletenessUnchecked = errors.New("completeness couldn't be checked")

// ErrUnknownObligation is returned by Waive and Attest for an obligation the
// session doesn't have, or one that no longer applies.
var ErrUnknownObligation = errors.New("unknown obligation")

// WithGate sets the configured completion gate. It is a floor: a manifest's
// gate can make it stricter, never looser, since the manifest lives in the
// repo the agent is editing. Without one, the floor is DefaultGate.
func (s *Service) WithGate(g GateMode) *Service {
	s.gate = g
	return s
}

// gateFor returns the gate for an evaluation: the stricter of the
// configured gate and the manifest's. An invalid manifest's gate can't be
// read, so the configured gate applies.
func (s *Service) gateFor(st *completenessState) GateMode {
	floor := s.configuredGate()
	if st.eval.Error != "" {
		return floor
	}
	return GateMode(st.eval.Gate).stricter(floor)
}

// configuredGate is the gate from config, or DefaultGate. A gate that
// slipped past ParseGateMode fails closed as GateBlock.
func (s *Service) configuredGate() GateMode {
	if s.gate != "" {
		return s.gate.failClosed()
	}
	return DefaultGate
}

// applyGate decides what Stop(completed) does with the required obligations
// open in st. It returns the status role to enqueue in place of complete
// (empty to keep it). Under the block gate it returns an
// *IncompleteSessionError unless force is set, in which case it records the
// override as a decision.
func (s *Service) applyGate(ctx context.Context, sess *Session, st *completenessState, force bool) (*GateResult, StatusRole, error) {
	g := &GateResult{Mode: s.gateFor(st), Outcome: GatePassed}
	switch {
	case st.readErr != "":
		return s.uncheckedGate(ctx, sess, g.Mode, st.readErr, nil, force)
	case st.eval.Error != "":
		// The stored obligations are the last known state; list what they
		// still owe, but new work since then can't be checked.
		reason := fmt.Sprintf("the completeness manifest %s is invalid: %s", st.eval.Manifest, st.eval.Error)
		return s.uncheckedGate(ctx, sess, g.Mode, reason, st.views(st.openRequired(), false), force)
	}
	open := st.openRequired()
	if len(open) == 0 {
		return g, "", nil
	}
	g.OpenRequired = st.views(open, false)
	switch g.Mode {
	case GateBlock:
		if !force {
			// A refused stop writes nothing, reminder counts included.
			e := &IncompleteSessionError{SessionID: sess.ID, Open: g.OpenRequired}
			for _, o := range open {
				if st.unsaved[o.ID] {
					e.Unrecorded = append(e.Unrecorded, o.ID)
				}
			}
			return nil, "", e
		}
		var describes, subjects []string
		for _, o := range open {
			describes = append(describes, o.Describe)
			subjects = append(subjects, o.Subject)
		}
		d, err := s.createDecision(ctx, sess, "Completion gate override",
			fmt.Sprintf("Completion forced by human with %d open obligations", len(open)),
			"Open: "+strings.Join(describes, "; "), "accepted", subjects)
		if err != nil {
			return nil, "", fmt.Errorf("failed to record forced completion: %w", err)
		}
		g.Outcome, g.DecisionID = GateForced, d.ID
	case GateReview:
		g.Outcome = GateMovedToReview
		return g, RoleReview, nil
	default:
		g.Outcome = GateReported
	}
	return g, "", nil
}

// uncheckedGate applies gate mode when completeness couldn't be checked
// (reason says why), acting as if required work may be open: block refuses
// with ErrCompletenessUnchecked unless force is set (then the override is
// recorded as a decision), review moves the task to review, and report
// completes. open lists required obligations known to be open, if any.
func (s *Service) uncheckedGate(ctx context.Context, sess *Session, mode GateMode, reason string, open []ObligationView, force bool) (*GateResult, StatusRole, error) {
	g := &GateResult{Mode: mode, OpenRequired: open, Unchecked: reason}
	switch mode {
	case GateBlock:
		if !force {
			return nil, "", fmt.Errorf("%w: cannot complete session %s under the block gate: %s. Fix the problem and stop again",
				ErrCompletenessUnchecked, sess.ID, reason)
		}
		d, err := s.createDecision(ctx, sess, "Completion gate override",
			"Completion forced by human without a completeness check",
			"Completeness couldn't be checked: "+reason, "accepted", nil)
		if err != nil {
			return nil, "", fmt.Errorf("failed to record forced completion: %w", err)
		}
		g.Outcome, g.DecisionID = GateForced, d.ID
		return g, "", nil
	case GateReview:
		g.Outcome = GateMovedToReview
		return g, RoleReview, nil
	}
	g.Outcome = GateReported
	return g, "", nil
}

// commentLine is the gate's line in the Session Finished comment, or "" when
// nothing required was open.
func (g *GateResult) commentLine() string {
	n := len(g.OpenRequired)
	if g.Unchecked != "" {
		switch g.Outcome {
		case GateReported:
			return "**Completion gate (report):** completed without a completeness check: " + g.Unchecked
		case GateMovedToReview:
			return "**Completion gate (review):** completeness couldn't be checked, so the task goes to review instead of complete: " + g.Unchecked
		case GateForced:
			return "**Completion gate (block):** completion forced by human; completeness couldn't be checked: " + g.Unchecked
		}
	}
	switch g.Outcome {
	case GateReported:
		return fmt.Sprintf("**Completion gate (report):** completed with %d required obligation(s) open", n)
	case GateMovedToReview:
		return fmt.Sprintf("**Completion gate (review):** %d required obligation(s) open, so the task goes to review instead of complete", n)
	case GateForced:
		return fmt.Sprintf("**Completion gate (block):** completion forced by human with %d open obligation(s)", n)
	}
	return ""
}

// ObligationResult is an obligation after tf_waive or tf_attest, with what
// the session still owes.
type ObligationResult struct {
	Obligation ObligationView `json:"obligation"`
	// DecisionID is the decision recording a waiver.
	DecisionID string              `json:"decision_id,omitempty"`
	Summary    CompletenessSummary `json:"summary"`
	StillOpen  []ObligationView    `json:"still_open"`
}

// Waive marks an obligation as not applying, for a reason recorded as a
// decision (and so posted to the ClickUp task). Open and attested
// obligations can be waived.
func (s *Service) Waive(ctx context.Context, sessionID, obligationID, rationale string) (*ObligationResult, error) {
	sess, err := s.activeSession(ctx, sessionID, "waive an obligation")
	if err != nil {
		return nil, err
	}
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return nil, errors.New("a rationale is required to waive an obligation")
	}
	st, o, err := s.obligationFor(ctx, sess, obligationID)
	if err != nil {
		return nil, err
	}
	switch o.Status {
	case ObligationSatisfied:
		return nil, st.obligationError(obligationID, "is already satisfied; there is nothing to waive")
	case ObligationWaived:
		return nil, st.obligationError(obligationID, "is already waived")
	}

	d, err := s.createDecision(ctx, sess, "Completeness waiver: "+o.Describe,
		fmt.Sprintf("Waived %s/%s for %s", o.RuleID, o.RequirementID, o.Subject),
		rationale, "rejected_alternative", []string{o.Subject})
	if err != nil {
		return nil, err
	}
	if err := s.store.WaiveObligation(ctx, o.ID, d.ID, rationale, time.Now()); err != nil {
		return nil, fmt.Errorf("failed to waive obligation: %w", err)
	}
	o.Status, o.WaiverDecisionID, o.Evidence = ObligationWaived, d.ID, rationale
	res := s.obligationResult(ctx, st, o)
	res.DecisionID = d.ID
	return res, nil
}

// ErrNotAttestable is returned by Attest for an obligation whose requirement
// asks for no attestation: it is checked from the project's files, so the
// work must be done, or the obligation waived with a rationale (a decision
// posted to ClickUp). An attestation must never stand in for either.
var ErrNotAttestable = errors.New("obligation can't be attested")

// Attest records the agent's evidence that an obligation is met, for the
// checks taskflow can't make itself. Only an obligation whose requirement
// asks for an attestation (its current finding has an attest prompt) can be
// attested; it is then attested, and satisfied once its file checks pass
// too. Open, attested (to replace the evidence), and waived (to take back
// the waiver) obligations can be attested. Attest evaluates the session
// once, and refuses (wrapping ErrCompletenessUnchecked) when it can't: no
// evaluator or manifest, an invalid manifest, or unreadable obligations.
func (s *Service) Attest(ctx context.Context, sessionID, obligationID, evidence string) (*ObligationResult, error) {
	sess, err := s.activeSession(ctx, sessionID, "attest an obligation")
	if err != nil {
		return nil, err
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return nil, errors.New("evidence is required to attest an obligation")
	}

	// Evaluate first: only the current finding says whether the requirement
	// asks for an attestation, and whether its checks pass.
	unchecked := func(reason string) error {
		return fmt.Errorf("%w: cannot attest obligation %q: %s", ErrCompletenessUnchecked, obligationID, reason)
	}
	if s.completeness == nil {
		return nil, unchecked("completeness is not enabled")
	}
	st, err := s.tryEvaluateCompleteness(ctx, sess, true)
	switch {
	case err != nil:
		return nil, unchecked("the evaluation failed: " + err.Error())
	case st == nil:
		return nil, unchecked("the project has no completeness manifest")
	case st.readErr != "":
		return nil, unchecked(st.readErr)
	case st.eval.Error != "":
		return nil, unchecked(fmt.Sprintf("the completeness manifest %s is invalid: %s", st.eval.Manifest, st.eval.Error))
	}
	o, err := st.resolvable(sess.ID, obligationID)
	if err != nil {
		return nil, err
	}
	if o.Status == ObligationSatisfied && !o.attested() {
		return nil, st.obligationError(obligationID, "is already satisfied by its checks; there is nothing to attest")
	}
	f := st.eval.finding(o)
	switch {
	case f == nil:
		return nil, fmt.Errorf("%w: obligation %q (%s) wasn't checked in this evaluation (its subject isn't among the session's changes), so it can't be attested; do the work, or call tf_waive with a rationale if it doesn't apply",
			ErrNotAttestable, obligationID, o.Describe)
	case f.AttestPrompt == "":
		msg := fmt.Sprintf("obligation %q (%s) is checked from the project's files and asks for no attestation", obligationID, o.Describe)
		if len(o.Expected) > 0 {
			msg += " (expected: " + strings.Join(o.Expected, ", ") + ")"
		}
		return nil, fmt.Errorf("%w: %s; do the work, or call tf_waive with a rationale if it doesn't apply", ErrNotAttestable, msg)
	}

	// With the attestation on record, passing checks satisfy the obligation;
	// otherwise the claim stands alone (as mergeObligations would decide).
	status, levels, now := ObligationAttested, withAttest(f.Levels), time.Now()
	if f.ChecksPassed {
		status = ObligationSatisfied
	}
	if err := s.store.AttestObligation(ctx, o.ID, evidence, levels, status, now); err != nil {
		return nil, fmt.Errorf("failed to attest obligation: %w", err)
	}
	o.Status, o.Evidence, o.VerifiedLevels, o.UpdatedAt = status, evidence, levels, now
	o.WaiverDecisionID = "" // an attestation takes back a waiver
	o.SatisfiedAt = nil
	if status == ObligationSatisfied {
		o.SatisfiedAt = &now
	}
	return s.obligationResult(ctx, st, o), nil
}

// finding returns the evaluation's finding for o, or nil when o wasn't
// checked. Like mergeObligations, the first finding for a key wins.
func (ev *Evaluation) finding(o *Obligation) *CompletenessFinding {
	k := o.key()
	for i := range ev.Findings {
		if ev.Findings[i].key() == k {
			return &ev.Findings[i]
		}
	}
	return nil
}

// activeSession returns the session, which must be active for action.
func (s *Service) activeSession(ctx context.Context, sessionID, action string) (*Session, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session: %w", ErrSessionNotFound)
	}
	if sess.Status != StatusActive {
		return nil, fmt.Errorf("cannot %s for non-active session (status: %s)", action, sess.Status)
	}
	return sess, nil
}

// obligationFor returns the session's stored obligations and the one with
// id, which must not be void.
func (s *Service) obligationFor(ctx context.Context, sess *Session, id string) (*completenessState, *Obligation, error) {
	stored, err := s.store.ListObligations(ctx, sess.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list obligations: %w", err)
	}
	st := &completenessState{eval: &Evaluation{}, obligations: stored, attest: map[string]string{}}
	o, err := st.resolvable(sess.ID, id)
	if err != nil {
		return nil, nil, err
	}
	return st, o, nil
}

// resolvable returns the session's obligation with id, which must not be
// void.
func (st *completenessState) resolvable(sessionID, id string) (*Obligation, error) {
	o := st.find(id)
	switch {
	case o == nil:
		return nil, st.obligationError(id, "is not an obligation of session "+sessionID)
	case o.Status == ObligationVoid:
		return nil, st.obligationError(id, "no longer applies (its trigger file was reverted)")
	}
	return o, nil
}

// obligationError is an ErrUnknownObligation error naming the obligations
// that can be resolved, so the agent can retry with the right id.
func (st *completenessState) obligationError(id, problem string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "obligation %q %s", id, problem)
	open := st.open()
	if len(open) == 0 {
		b.WriteString("; the session has no open obligations")
	} else {
		b.WriteString("; open obligations:")
		for _, o := range open {
			fmt.Fprintf(&b, "\n  - %s: %s [%s/%s, %s]", o.ID, o.Describe, o.RuleID, o.RequirementID, o.Severity)
		}
	}
	return fmt.Errorf("%w: %s", ErrUnknownObligation, b.String())
}

func (s *Service) obligationResult(ctx context.Context, st *completenessState, o *Obligation) *ObligationResult {
	res := &ObligationResult{
		Obligation: st.view(o, true),
		Summary:    st.summary(),
		StillOpen:  st.views(st.open(), false),
	}
	s.remind(ctx, st)
	return res
}

// PlanResult is the session's plan state after tf_plan, with the
// obligations the plan created.
type PlanResult struct {
	PlanState PlanState        `json:"plan_state"`
	Planned   []ObligationView `json:"planned,omitempty"`
	// DecisionID is the decision recording a declined plan.
	DecisionID string              `json:"decision_id,omitempty"`
	Summary    CompletenessSummary `json:"summary"`
	Warnings   []string            `json:"warnings,omitempty"`
}

// Plan declares work the agent expects to do before its files exist, so its
// obligations are tracked from the start: each item names a plannable rule
// and the vars that render its subject file. Each planned subject gets the
// rule's obligations plus one for the file itself. With declineReason
// instead, it records (as a decision) that no plan applies, which stops the
// plan reminders.
func (s *Service) Plan(ctx context.Context, sessionID string, items []PlanItem, declineReason string) (*PlanResult, error) {
	sess, err := s.activeSession(ctx, sessionID, "plan")
	if err != nil {
		return nil, err
	}
	declineReason = strings.TrimSpace(declineReason)
	switch {
	case len(items) > 0 && declineReason != "":
		return nil, errors.New("give plan items or a decline reason, not both")
	case len(items) == 0 && declineReason == "":
		return nil, errors.New("give plan items, or a decline reason when no plan applies")
	case s.completeness == nil:
		return nil, errors.New("completeness is not enabled")
	}

	if declineReason != "" {
		if sess.PlanState == PlanPlanned {
			return nil, errors.New("the session already has a plan; waive its obligations instead of declining")
		}
		d, err := s.createDecision(ctx, sess, "Completeness plan declined", "No up-front plan for this task",
			declineReason, "rejected_alternative", nil)
		if err != nil {
			return nil, err
		}
		if err := s.store.SetSessionPlanState(ctx, sess.ID, PlanDeclined); err != nil {
			return nil, fmt.Errorf("failed to save plan state: %w", err)
		}
		return &PlanResult{PlanState: PlanDeclined, DecisionID: d.ID}, nil
	}

	stored, err := s.store.ListObligations(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list obligations: %w", err)
	}
	ev, err := s.completeness.Plan(ctx, sess, stored, items)
	switch {
	case err != nil:
		return nil, err
	case ev == nil:
		return nil, fmt.Errorf("%w: the project has no completeness manifest", ErrInvalidPlan)
	case ev.Error != "":
		return nil, fmt.Errorf("%w: the completeness manifest %s is invalid: %s", ErrInvalidPlan, ev.Manifest, ev.Error)
	}
	if err := checkPlanFindings(ev); err != nil {
		return nil, err
	}
	// The plan is only recorded once all of its obligations are: a plan
	// state with obligations missing would claim work is tracked that isn't.
	st, mr := mergeEvaluation(sess, stored, ev)
	if err := s.persistMerge(ctx, st, mr); err != nil {
		return nil, fmt.Errorf("failed to save the plan's obligations, so the plan wasn't recorded; call tf_plan again: %w", err)
	}
	if err := s.store.SetSessionPlanState(ctx, sess.ID, PlanPlanned); err != nil {
		return nil, fmt.Errorf("failed to save plan state: %w", err)
	}

	subjects := map[string]bool{}
	for _, p := range ev.Planned {
		subjects[subjectKey(p.RuleID, p.Subject)] = true
	}
	var planned []*Obligation
	for _, o := range st.visible() {
		if subjects[o.subjectKey()] {
			planned = append(planned, o)
		}
	}
	res := &PlanResult{
		PlanState: PlanPlanned,
		Planned:   st.views(planned, true),
		Summary:   st.summary(),
		Warnings:  ev.Warnings,
	}
	s.remind(ctx, st)
	return res, nil
}

// checkPlanFindings returns an error wrapping ErrInvalidPlan when a planned
// subject has no findings: nothing would track it, so the plan would claim
// work it never checks. A stack-scoped rule finds nothing when its stack
// isn't detected (its root is missing), so the error says so.
func checkPlanFindings(ev *Evaluation) error {
	found := map[string]bool{}
	for i := range ev.Findings {
		found[subjectKey(ev.Findings[i].RuleID, ev.Findings[i].Subject)] = true
	}
	stacks := map[string]string{}
	for _, r := range ev.Rules {
		stacks[r.ID] = r.Stack
	}
	seen := map[string]bool{}
	var problems []string
	for _, p := range ev.Planned {
		k := subjectKey(p.RuleID, p.Subject)
		if found[k] || seen[k] {
			continue
		}
		seen[k] = true
		problem := fmt.Sprintf("rule %q yields no obligations for %s", p.RuleID, p.Subject)
		if stack := stacks[p.RuleID]; stack != "" {
			problem += fmt.Sprintf(" (the rule is scoped to stack %q, which may not be detected in the project: check that its root exists)", stack)
		}
		problems = append(problems, problem)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; nothing was planned", ErrInvalidPlan, strings.Join(problems, "; "))
}

// SuggestedPlan suggests declaring the task's expected work with tf_plan,
// because the task names something a plannable rule covers.
type SuggestedPlan struct {
	Reason  string    `json:"reason"`
	Rules   []string  `json:"rules"`
	Example *PlanItem `json:"example"`
}

// suggestPlan matches the plannable rules' plan_hints against the task's
// name and description (never its tags), as whole words, case-insensitive,
// allowing a plural: "screen" matches "Screens" but not "screenshot". It
// returns nil when nothing matches.
func suggestPlan(rules []RuleInfo, taskName, taskDescription string) *SuggestedPlan {
	name, desc := words(taskName), words(taskDescription)
	sp := &SuggestedPlan{}
	for _, r := range rules {
		if !r.Plannable {
			continue
		}
		for _, hint := range r.PlanHints {
			where := ""
			switch {
			case containsPhrase(name, words(hint)):
				where = "name"
			case containsPhrase(desc, words(hint)):
				where = "description"
			default:
				continue
			}
			if sp.Reason == "" {
				sp.Reason = fmt.Sprintf("Task %s mentions %q", where, hint)
				vars := map[string]string{}
				for _, v := range r.PlanVars {
					vars[v] = "<" + v + ">"
				}
				sp.Example = &PlanItem{RuleID: r.ID, Vars: vars}
			}
			sp.Rules = append(sp.Rules, r.ID)
			break
		}
	}
	if len(sp.Rules) == 0 {
		return nil
	}
	return sp
}

// planReminder is the checkpoint's reminder while a suggested plan is
// neither made nor declined.
func planReminder(rules []RuleInfo) string {
	var plannable []string
	for _, r := range rules {
		if r.Plannable {
			plannable = append(plannable, r.ID)
		}
	}
	return fmt.Sprintf("No plan yet: if this task adds work a plannable rule covers (%s), call tf_plan with items [{rule_id, vars}] so its obligations are tracked before the files exist; otherwise call tf_plan with decline_reason.",
		strings.Join(plannable, ", "))
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsPhrase reports whether phrase occurs in text as consecutive words;
// its last word may also take a plural "s" or "es".
func containsPhrase(text, phrase []string) bool {
	if len(phrase) == 0 {
		return false
	}
	for i := 0; i+len(phrase) <= len(text); i++ {
		match := true
		for j, w := range phrase {
			got := text[i+j]
			if j == len(phrase)-1 {
				match = got == w || got == w+"s" || got == w+"es"
			} else if got != w {
				match = false
			}
			if !match {
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// inheritObligations copies the open, attested, and waived obligations of
// the task's finished session prev into the new session sess.
func (s *Service) inheritObligations(ctx context.Context, prev, sess *Session) {
	if s.completeness == nil {
		return
	}
	if err := s.store.InheritObligations(ctx, prev.ID, sess.ID); err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to inherit obligations from session %s: %v\n", prev.ID, err)
	}
}

// inheritedPlanState is the plan state a new session starts with after the
// task's finished session prev: a plan or a declined plan is a decision
// already made for the task, so it carries over instead of being suggested
// again. Anything else starts from PlanNone.
func inheritedPlanState(prev *Session) PlanState {
	if prev != nil && (prev.PlanState == PlanPlanned || prev.PlanState == PlanDeclined) {
		return prev.PlanState
	}
	return PlanNone
}
