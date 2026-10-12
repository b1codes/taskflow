package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrCommandsDisabled is returned by Verify while command checks are off.
var ErrCommandsDisabled = errors.New("command checks are disabled")

// Verify runs the session's command checks now, rather than waiting for a
// completing stop, and returns the full report with the runs. Commands come
// only from the project's manifest and overrides. It needs command checks
// enabled (completeness.allow_commands in config), and refuses when
// completeness can't be checked.
func (s *Service) Verify(ctx context.Context, sessionID string) (*CompletenessReport, error) {
	sess, err := s.activeSession(ctx, sessionID, "verify")
	if err != nil {
		return nil, err
	}
	if s.completeness == nil {
		return nil, errors.New("completeness is not enabled")
	}
	st, mr, err := s.evaluateUnsaved(ctx, sess, true)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: the evaluation failed: %v", ErrCompletenessUnchecked, err)
	case st == nil:
		return nil, errors.New("the project has no completeness manifest (.agents/completeness.yaml or .taskflow/completeness.yaml)")
	case st.readErr != "":
		return nil, fmt.Errorf("%w: %s", ErrCompletenessUnchecked, st.readErr)
	case st.eval.Error != "":
		return nil, fmt.Errorf("%w: the completeness manifest %s is invalid: %s", ErrCompletenessUnchecked, st.eval.Manifest, st.eval.Error)
	case !st.eval.CommandsEnabled:
		return nil, fmt.Errorf("%w: set allow_commands = true under [completeness] in ~/.taskflow/config.toml to run them; until then, obligations that name a command ask for an attestation instead", ErrCommandsDisabled)
	}
	_ = s.persistMerge(ctx, sess, st, mr)
	r := st.report(sess.ID)
	if len(st.runs) == 0 {
		r.Warnings = append(r.Warnings, "no command checks apply to this session's obligations")
	}
	s.remind(ctx, st)
	return r, nil
}

// CheckResult is whether a session's required work is done, for hooks and
// git hooks that gate on it (taskflow check).
type CheckResult struct {
	SessionID string `json:"session_id"`
	Status    Status `json:"status"`
	// Blocked means the session is active and required obligations are open,
	// or completeness couldn't be checked (Unchecked says why), which counts
	// as work that may be open, as at the completion gate.
	Blocked      bool             `json:"blocked"`
	OpenRequired []ObligationView `json:"open_required"`
	Unchecked    string           `json:"unchecked,omitempty"`
	// Report is nil when the project has no manifest.
	Report *CompletenessReport `json:"report,omitempty"`
}

// Check evaluates an active session and reports whether required work is
// open. A session that isn't active never blocks: its obligations are
// reported as recorded. remind counts the open obligations as returned to
// the agent (an agent-tool hook), as tf_completeness does. Command checks
// aren't run; a pass recorded by tf_verify counts.
func (s *Service) Check(ctx context.Context, sessionID string, remind bool) (*CheckResult, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session: %w", ErrSessionNotFound)
	}
	res := &CheckResult{SessionID: sess.ID, Status: sess.Status, OpenRequired: []ObligationView{}}
	if sess.Status != StatusActive {
		r, err := s.storedCompleteness(ctx, sess)
		if err != nil {
			return nil, err
		}
		if r.Enabled {
			res.Report = r
		}
		return res, nil
	}
	st, err := s.tryEvaluateCompleteness(ctx, sess, true)
	switch {
	case err != nil:
		res.Blocked, res.Unchecked = true, "the completeness evaluation failed: "+err.Error()
		return res, nil
	case st == nil:
		return res, nil
	}
	res.Report = st.report(sess.ID)
	res.OpenRequired = st.views(st.openRequired(), false)
	if text := st.errText(); text != "" {
		res.Unchecked = text
		if st.eval.Error != "" {
			res.Unchecked = fmt.Sprintf("the completeness manifest %s is invalid: %s", st.eval.Manifest, text)
		}
	}
	res.Blocked = len(res.OpenRequired) > 0 || res.Unchecked != ""
	if remind {
		s.remind(ctx, st)
	}
	return res, nil
}

// Reason explains a blocked check to the agent: what is open and how to
// resolve it.
func (r *CheckResult) Reason() string {
	var b strings.Builder
	if r.Unchecked != "" {
		fmt.Fprintf(&b, "Taskflow couldn't check this session's completeness: %s.", r.Unchecked)
		if len(r.OpenRequired) > 0 {
			b.WriteString(" Last known open required work:")
		}
	} else {
		fmt.Fprintf(&b, "Taskflow session %s still has %d required obligation(s) open:", r.SessionID, len(r.OpenRequired))
	}
	for _, o := range r.OpenRequired {
		fmt.Fprintf(&b, "\n- %s [%s]", o.Describe, o.ObligationID)
		switch {
		case o.Command != nil:
			fmt.Fprintf(&b, " (command `%s` in %s: %s)", o.Command.Command, o.Command.Root, o.Command.Result)
		case o.Expected != "":
			fmt.Fprintf(&b, " (expected: %s)", o.Expected)
		}
	}
	b.WriteString("\nDo the work (tf_verify runs command checks), attest it with tf_attest if it asks for an attestation, or call tf_waive with a rationale if it doesn't apply. Then finish.")
	return b.String()
}
