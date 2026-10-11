package session

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// DefaultGate applies when neither config nor the manifest sets a gate.
const DefaultGate = GateReview

// ParseGateMode validates a gate name; "" is the default gate.
func ParseGateMode(s string) (GateMode, error) {
	switch g := GateMode(s); g {
	case "":
		return DefaultGate, nil
	case GateReport, GateReview, GateBlock:
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
	// Unchecked says why completeness couldn't be checked, when a forced
	// completion went ahead without knowing what was open.
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
// while required obligations are open; the session is left unchanged.
var ErrIncompleteSession = errors.New("session has open required obligations")

// IncompleteSessionError lists the obligations holding up completion.
type IncompleteSessionError struct {
	SessionID string
	Open      []ObligationView
}

func (e *IncompleteSessionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot complete session %s: %d required obligation(s) open (completion gate: block):", e.SessionID, len(e.Open))
	for _, o := range e.Open {
		fmt.Fprintf(&b, "\n  - %s: %s", o.ObligationID, o.Describe)
		if o.Expected != "" {
			fmt.Fprintf(&b, " (expected: %s)", o.Expected)
		}
	}
	b.WriteString("\nDo the work, attest it with tf_attest, or waive it with tf_waive and a rationale, then stop again.")
	return b.String()
}

func (e *IncompleteSessionError) Unwrap() error { return ErrIncompleteSession }

// ErrCompletenessUnchecked is returned by Stop(completed) under the block
// gate when the session's obligations can't be checked (they can't be read,
// or the evaluation failed). The gate fails closed: the session is left
// unchanged, since nothing shows the required work is done.
var ErrCompletenessUnchecked = errors.New("completeness couldn't be checked")

// ErrUnknownObligation is returned by Waive and Attest for an obligation the
// session doesn't have, or one that no longer applies.
var ErrUnknownObligation = errors.New("unknown obligation")

// WithGate sets the configured completion gate; a manifest's gate overrides
// it. Without one, the gate is DefaultGate.
func (s *Service) WithGate(g GateMode) *Service {
	s.gate = g
	return s
}

// gateFor returns the gate for an evaluation: the manifest's, else the
// configured one. An invalid manifest's rules (and gate) can't be read, so
// its gate is report. Unreadable obligations don't change the gate: under
// block, applyGate refuses to complete without them.
func (s *Service) gateFor(st *completenessState) GateMode {
	if st.eval.Error != "" {
		return GateReport
	}
	if st.eval.Gate != "" {
		return GateMode(st.eval.Gate)
	}
	return s.configuredGate()
}

// configuredGate is the gate from config, or DefaultGate.
func (s *Service) configuredGate() GateMode {
	if s.gate != "" {
		return s.gate
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
	if st.readErr != "" {
		// What's open is unknown. Only block depends on it, and it fails
		// closed; other gates complete as they would with nothing open.
		if g.Mode == GateBlock {
			return s.blockUnchecked(ctx, sess, st.readErr, force)
		}
		g.Mode = GateReport
		return g, "", nil
	}
	open := st.openRequired()
	if len(open) == 0 {
		return g, "", nil
	}
	g.OpenRequired = st.views(open, false)
	switch g.Mode {
	case GateBlock:
		if !force {
			s.remind(ctx, st)
			return nil, "", &IncompleteSessionError{SessionID: sess.ID, Open: g.OpenRequired}
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

// blockUnchecked applies the block gate when completeness couldn't be
// checked (reason says why): it refuses with ErrCompletenessUnchecked unless
// force is set, in which case it records the override as a decision.
func (s *Service) blockUnchecked(ctx context.Context, sess *Session, reason string, force bool) (*GateResult, StatusRole, error) {
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
	return &GateResult{Mode: GateBlock, Outcome: GateForced, DecisionID: d.ID, Unchecked: reason}, "", nil
}

// commentLine is the gate's line in the Session Finished comment, or "" when
// nothing required was open.
func (g *GateResult) commentLine() string {
	n := len(g.OpenRequired)
	switch g.Outcome {
	case GateReported:
		return fmt.Sprintf("**Completion gate (report):** completed with %d required obligation(s) open", n)
	case GateMovedToReview:
		return fmt.Sprintf("**Completion gate (review):** %d required obligation(s) open, so the task goes to review instead of complete", n)
	case GateForced:
		if g.Unchecked != "" {
			return "**Completion gate (block):** completion forced by human; completeness couldn't be checked: " + g.Unchecked
		}
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

// Attest records the agent's evidence that an obligation is met, for the
// checks taskflow can't make itself. The obligation is attested, and
// satisfied once its file checks pass too. Open, attested (to replace the
// evidence), and waived (to take back the waiver) obligations can be
// attested.
func (s *Service) Attest(ctx context.Context, sessionID, obligationID, evidence string) (*ObligationResult, error) {
	sess, err := s.activeSession(ctx, sessionID, "attest an obligation")
	if err != nil {
		return nil, err
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return nil, errors.New("evidence is required to attest an obligation")
	}
	st, o, err := s.obligationFor(ctx, sess, obligationID)
	if err != nil {
		return nil, err
	}
	if o.Status == ObligationSatisfied && !o.attested() {
		return nil, st.obligationError(obligationID, "is already satisfied by its checks; there is nothing to attest")
	}

	if err := s.store.AttestObligation(ctx, o.ID, evidence, withAttest(o.VerifiedLevels), ObligationAttested, time.Now()); err != nil {
		return nil, fmt.Errorf("failed to attest obligation: %w", err)
	}
	// Re-evaluate: with the attestation on record, passing checks now
	// satisfy the obligation.
	if next := s.evaluateCompleteness(ctx, sess, true); next != nil && next.readErr == "" {
		st = next
	} else {
		o.Status, o.Evidence, o.VerifiedLevels = ObligationAttested, evidence, withAttest(o.VerifiedLevels)
	}
	if found := st.find(o.ID); found != nil {
		o = found
	}
	return s.obligationResult(ctx, st, o), nil
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
	o := st.find(id)
	switch {
	case o == nil:
		return nil, nil, st.obligationError(id, "is not an obligation of session "+sess.ID)
	case o.Status == ObligationVoid:
		return nil, nil, st.obligationError(id, "no longer applies (its trigger file was reverted)")
	}
	return st, o, nil
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
	st := s.applyEvaluation(ctx, sess, stored, ev)
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
