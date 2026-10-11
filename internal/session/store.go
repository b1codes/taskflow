package session

import (
	"context"
	"errors"
	"time"
)

// ErrSessionNotFound is returned by SessionStore lookup methods when no matching session exists.
var ErrSessionNotFound = errors.New("session not found")

type SessionStore interface {
	CreateSession(ctx context.Context, s *Session) error

	// GetSession returns the session by its ID, or ErrSessionNotFound if not found.
	GetSession(ctx context.Context, id string) (*Session, error)

	// GetSessionByTaskID returns the most recent session for a given task ID, or ErrSessionNotFound if not found.
	GetSessionByTaskID(ctx context.Context, taskID string) (*Session, error)

	// GetActiveSessionByProject returns the active session for a project path, or ErrSessionNotFound if not found.
	GetActiveSessionByProject(ctx context.Context, projectPath string) (*Session, error)

	UpdateSessionStatus(ctx context.Context, id string, status Status) error
	UpdateSessionGitBranch(ctx context.Context, id string, branch string) error
	UpdateSessionTaskName(ctx context.Context, id string, taskName string) error
	UpdateSessionUpdatedAt(ctx context.Context, id string, updatedAt time.Time) error
	ListSessions(ctx context.Context, filter SessionFilter) ([]*Session, error)
	CreateCheckpoint(ctx context.Context, cp *Checkpoint) error
	ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]*Checkpoint, error)
	CreateSnag(ctx context.Context, snag *Snag) error
	ListSnagsBySession(ctx context.Context, sessionID string) ([]*Snag, error)
	FindMatchingResolutions(ctx context.Context, signature string) ([]*Snag, error)
	UpdateSnagResolution(ctx context.Context, id string, resolution string, resolvedAt time.Time) error
	CreateDecision(ctx context.Context, d *Decision) error
	ListDecisionsBySession(ctx context.Context, sessionID string) ([]*Decision, error)
	CountPendingByTaskID(ctx context.Context, taskID string) (int, error)
	EnqueueSync(ctx context.Context, op SyncOp) error

	SetSessionBaseSHA(ctx context.Context, id string, sha string) error
	// UpsertObligation inserts o, or updates the obligation with the same
	// (session, rule, requirement, subject) key, keeping that one's ID,
	// first_seen_at, reminder count, evidence, and waiver. It sets o.ID to the
	// stored row's ID.
	UpsertObligation(ctx context.Context, o *Obligation) error
	ListObligations(ctx context.Context, sessionID string) ([]*Obligation, error)
	// UpdateObligationStatus sets satisfied_at to at when status is
	// satisfied, and clears it when status is open.
	UpdateObligationStatus(ctx context.Context, id string, status ObligationStatus, at time.Time) error
	IncrementObligationReminders(ctx context.Context, ids []string) error

	SetSessionPlanState(ctx context.Context, id string, state PlanState) error
	// AttestObligation records an attestation: the evidence, the verified
	// levels (which include "attest"), and the resulting status.
	AttestObligation(ctx context.Context, id string, evidence string, levels []string, status ObligationStatus, at time.Time) error
	// WaiveObligation marks the obligation waived, linking the decision that
	// records why, and keeps the rationale as its evidence.
	WaiveObligation(ctx context.Context, id string, decisionID string, rationale string, at time.Time) error
	// InheritObligations copies fromSessionID's open, attested, and waived
	// obligations into toSessionID with new IDs and inherited_from set to the
	// source obligation, keeping status, verified levels, evidence, and
	// waiver links.
	InheritObligations(ctx context.Context, fromSessionID, toSessionID string) error
}
