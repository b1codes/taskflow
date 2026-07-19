package session

import (
	"context"
	"time"
)

type SessionStore interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	GetSessionByTaskID(ctx context.Context, taskID string) (*Session, error)
	GetActiveSessionByProject(ctx context.Context, projectPath string) (*Session, error)
	UpdateSessionStatus(ctx context.Context, id string, status Status) error
	UpdateSessionGitBranch(ctx context.Context, id string, branch string) error
	UpdateSessionUpdatedAt(ctx context.Context, id string, updatedAt time.Time) error
	ListSessions(ctx context.Context, filter SessionFilter) ([]*Session, error)
	CreateCheckpoint(ctx context.Context, cp *Checkpoint) error
	ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]*Checkpoint, error)
	CreateSnag(ctx context.Context, snag *Snag) error
	ListSnagsBySession(ctx context.Context, sessionID string) ([]*Snag, error)
	FindMatchingResolutions(ctx context.Context, signature string) ([]*Snag, error)
	UpdateSnagResolution(ctx context.Context, id string, resolution string, resolvedAt time.Time) error
	CountPendingByTaskID(ctx context.Context, taskID string) (int, error)
	EnqueueSync(ctx context.Context, op SyncOp) error
}
