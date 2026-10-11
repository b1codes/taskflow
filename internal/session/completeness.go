package session

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ObligationStatus is the state of one obligation. Attested and waived are
// set by tf_attest/tf_waive (Phase 2); evaluation never changes them.
type ObligationStatus string

const (
	ObligationOpen      ObligationStatus = "open"
	ObligationSatisfied ObligationStatus = "satisfied"
	ObligationAttested  ObligationStatus = "attested"
	ObligationWaived    ObligationStatus = "waived"
	ObligationVoid      ObligationStatus = "void"
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
	FirstSeenAt      time.Time        `json:"first_seen_at"`
	SatisfiedAt      *time.Time       `json:"satisfied_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

func (o *Obligation) key() string {
	return o.RuleID + "\x00" + o.RequirementID + "\x00" + o.Subject
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
	Satisfied     bool
	Levels        []string
	// AttestPrompt is set when the requirement needs an attestation.
	AttestPrompt string
}

func (f *CompletenessFinding) key() string {
	return f.RuleID + "\x00" + f.RequirementID + "\x00" + f.Subject
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
	// weren't found are kept rather than voided.
	Partial  bool
	Warnings []string
}

// CompletenessEvaluator checks a session's project against its completeness
// manifest. A nil evaluator turns completeness off.
type CompletenessEvaluator interface {
	// BaseSHA returns the commit a new session's changes are measured from.
	BaseSHA(projectPath string) (string, error)
	// Evaluate returns nil, nil when the project has no manifest. stored are
	// the session's obligations: when the changed set is partial, open and
	// satisfied ones are verified again even if their subject isn't in it.
	Evaluate(ctx context.Context, sess *Session, stored []*Obligation) (*Evaluation, error)
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
}

// CheckpointCompleteness is what changed since the agent last heard about
// completeness, returned with each checkpoint.
type CheckpointCompleteness struct {
	Summary        CompletenessSummary `json:"summary"`
	NewlyOpen      []ObligationView    `json:"newly_open"`
	NewlySatisfied []string            `json:"newly_satisfied"`
	StillOpen      []string            `json:"still_open"`
	Error          string              `json:"error,omitempty"`
	Warnings       []string            `json:"warnings,omitempty"`
}

// ContractCompleteness is the start contract's completeness section. Phase 1
// carries the manifest and resolved stacks; open obligations, rules, and
// plan suggestions come with enforcement.
type ContractCompleteness struct {
	Manifest string      `json:"manifest"`
	Error    string      `json:"error,omitempty"`
	Stacks   []StackInfo `json:"stacks"`
	Warnings []string    `json:"warnings,omitempty"`
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
	Warnings    []string            `json:"warnings,omitempty"`
}

// mergeResult is an evaluation merged into a session's stored obligations.
type mergeResult struct {
	obligations    []*Obligation // every obligation of the session, after the merge
	upserts        []*Obligation // new or changed, to write
	voided         []*Obligation // to mark void
	newlyOpen      map[string]bool
	newlySatisfied []*Obligation
	attest         map[string]string // obligation ID → attest prompt
}

// mergeObligations folds an evaluation's findings into the stored
// obligations:
//   - a new key is inserted as open or satisfied;
//   - waived and attested obligations are left alone (sticky);
//   - other existing ones are recomputed open ⇄ satisfied (a void one that
//     is found again comes back);
//   - stored keys that weren't found are voided (e.g. the file was reverted),
//     unless they were planned or inherited, or the evaluation is partial.
func mergeObligations(sessionID string, stored []*Obligation, ev *Evaluation, now time.Time) *mergeResult {
	mr := &mergeResult{newlyOpen: map[string]bool{}, attest: map[string]string{}}
	byKey := make(map[string]*Obligation, len(stored))
	for _, o := range stored {
		byKey[o.key()] = o
		mr.obligations = append(mr.obligations, o)
	}

	found := map[string]bool{}
	for i := range ev.Findings {
		f := &ev.Findings[i]
		k := f.key()
		if found[k] {
			continue
		}
		found[k] = true
		status := ObligationOpen
		if f.Satisfied {
			status = ObligationSatisfied
		}

		o, ok := byKey[k]
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
				VerifiedLevels: f.Levels,
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
			continue
		}
		if f.AttestPrompt != "" {
			mr.attest[o.ID] = f.AttestPrompt
		}

		changed := o.Source != f.Source || o.Severity != f.Severity || o.Describe != f.Describe || !slices.Equal(o.Expected, f.Expected)
		o.Source, o.Severity, o.Describe, o.Expected = f.Source, f.Severity, f.Describe, f.Expected
		if o.Status != ObligationWaived && o.Status != ObligationAttested {
			prev := o.Status
			if !slices.Equal(o.VerifiedLevels, f.Levels) {
				o.VerifiedLevels = f.Levels
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

	if !ev.Partial {
		for _, o := range stored {
			if found[o.key()] || o.Planned || o.InheritedFrom != "" {
				continue
			}
			if o.Status == ObligationOpen || o.Status == ObligationSatisfied {
				o.Status = ObligationVoid
				o.UpdatedAt = now
				mr.voided = append(mr.voided, o)
			}
		}
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
	// readErr is set when the stored obligations couldn't be read; the state
	// then can't say what is owed, and must not claim nothing is.
	readErr string
}

// evaluateCompleteness evaluates sess and, when merge is set and the
// manifest is valid, writes the result to its obligations. It returns nil
// when completeness is off for the session (no evaluator or no manifest).
// Failures are logged and never fail the caller.
func (s *Service) evaluateCompleteness(ctx context.Context, sess *Session, merge bool) *completenessState {
	if s.completeness == nil {
		return nil
	}
	stored, listErr := s.store.ListObligations(ctx, sess.ID)
	if listErr != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to list obligations for session %s: %v\n", sess.ID, listErr)
	}
	ev, err := s.completeness.Evaluate(ctx, sess, stored)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[session] completeness evaluation failed for session %s: %v\n", sess.ID, err)
		return nil
	}
	if ev == nil {
		return nil
	}
	if listErr != nil {
		return &completenessState{eval: ev, readErr: "stored obligations couldn't be read: " + listErr.Error()}
	}
	st := &completenessState{eval: ev, obligations: stored, newlyOpen: map[string]bool{}, attest: map[string]string{}}
	if ev.Error != "" || !merge {
		return st
	}

	now := time.Now()
	mr := mergeObligations(sess.ID, stored, ev, now)
	for _, o := range mr.upserts {
		id := o.ID
		if err := s.store.UpsertObligation(ctx, o); err != nil {
			fmt.Fprintf(os.Stderr, "[session] failed to save obligation %s/%s for %s: %v\n", o.RuleID, o.RequirementID, o.Subject, err)
			continue
		}
		// Another evaluation (the MCP server and the CLI) inserted the key
		// first; the upsert kept its ID, so follow it.
		if o.ID != id {
			if mr.newlyOpen[id] {
				delete(mr.newlyOpen, id)
				mr.newlyOpen[o.ID] = true
			}
			if prompt, ok := mr.attest[id]; ok {
				delete(mr.attest, id)
				mr.attest[o.ID] = prompt
			}
		}
	}
	for _, o := range mr.voided {
		if err := s.store.UpdateObligationStatus(ctx, o.ID, ObligationVoid, now); err != nil {
			fmt.Fprintf(os.Stderr, "[session] failed to void obligation %s: %v\n", o.ID, err)
		}
	}
	st.obligations = mr.obligations
	st.newlyOpen = mr.newlyOpen
	st.newlySatisfied = mr.newlySatisfied
	st.attest = mr.attest
	return st
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
	if withStatus {
		v.Status = o.Status
	}
	return v
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

func (st *completenessState) contractView() *ContractCompleteness {
	stacks := st.eval.Stacks
	if stacks == nil {
		stacks = []StackInfo{}
	}
	return &ContractCompleteness{
		Manifest: st.eval.Manifest,
		Error:    st.eval.Error,
		Stacks:   stacks,
		Warnings: st.eval.Warnings,
	}
}

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
	}
	for _, o := range st.visible() {
		r.Obligations = append(r.Obligations, st.view(o, true))
	}
	return r
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
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s | %s |\n",
			cell(o.Describe), cell(o.Subject), o.Severity, o.Status, verified, cell(evidence))
	}
	return strings.TrimSuffix(b.String(), "\n")
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
