package store

import (
	"context"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

type CacheEntry struct {
	ID         int64     `json:"id"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	ParentID   string    `json:"parent_id"`
	Name       string    `json:"name"`
	StatusType string    `json:"status_type"`
	OrderIndex int       `json:"order_index"`
	Extra      string    `json:"extra"` // JSON string
	FetchedAt  time.Time `json:"fetched_at"`
}

type Store interface {
	session.SessionStore

	// ClickUp Cache
	UpsertCacheEntry(ctx context.Context, entry *CacheEntry) error
	GetCacheEntries(ctx context.Context, entityType, parentID string) ([]*CacheEntry, error)
	GetCacheEntry(ctx context.Context, entityType, entityID string) (*CacheEntry, error)
	GetListStatuses(ctx context.Context, listID string) ([]*CacheEntry, error)
	IsCacheStale(ctx context.Context, entityType, entityID string, ttl time.Duration) (bool, error)
	ClearCache(ctx context.Context) error
	// ReplaceListStatuses atomically replaces every cached status of listID
	// with entries, so readers never see a partial status set.
	ReplaceListStatuses(ctx context.Context, listID string, entries []*CacheEntry) error

	// Sync Queue
	DequeuePending(ctx context.Context, limit int) ([]*session.SyncOp, error)
	// MarkInFlight claims the op for processing if it is still waiting to run
	// (pending, or failed with retries left) and returns it as claimed, its
	// payload included, since CoalesceSyncOp may have replaced the payload
	// after it was listed. It returns nil when the op is no longer waiting:
	// another worker claimed it, or it finished.
	MarkInFlight(ctx context.Context, id int64) (*session.SyncOp, error)
	// MarkDone marks the op DONE; a non-empty note (e.g. items ClickUp
	// rejected) replaces its error_msg.
	MarkDone(ctx context.Context, id int64, note string) error
	// MarkPartial marks the op PARTIAL: it applied its state except for
	// something a later op of the same state can still apply (see
	// session.enqueueChecklist), noted in its error_msg. Like DONE, it counts
	// as applied for HasNewerOp and as history for later ops.
	MarkPartial(ctx context.Context, id int64, note string) error
	// MarkSkipped marks the op SKIPPED, finished without applying anything
	// because a later op superseded it (see HasNewerOp); reason is kept as
	// its error_msg.
	MarkSkipped(ctx context.Context, id int64, reason string) error
	MarkFailed(ctx context.Context, id int64, errMsg string) error
	// MarkFailedPermanent marks the op FAILED with retries set to max_retries
	// so ListRetryable never returns it, for errors that cannot succeed on
	// retry (e.g. no matching status on the list, or an invalid payload).
	MarkFailedPermanent(ctx context.Context, id int64, errMsg string) error
	RequeueRateLimited(ctx context.Context, id int64, errMsg string) error
	ListRetryable(ctx context.Context) ([]*session.SyncOp, error)
	// HasNewerOp reports whether a later op of the same operation for the
	// same task as op applied (DONE, PARTIAL) or still will run (pending, in
	// flight, or failed with retries left), so an op carrying full state can
	// be skipped for it: retrying it after a later one applied would undo the
	// later state. A later op that failed for good or was itself SKIPPED
	// doesn't count: it applied nothing. CoalesceSyncOp keeps at most one op
	// waiting per task, so a newer op only exists beside a waiting one when
	// the newer was queued while the older was in flight.
	HasNewerOp(ctx context.Context, op *session.SyncOp) (bool, error)
	// ListSyncOps lists the task's ops of operation with an ID below beforeID
	// (0 for no bound), newest first, at most limit of them.
	ListSyncOps(ctx context.Context, taskID, operation string, beforeID int64, limit int) ([]*session.SyncOp, error)
	ResetStaleInFlight(ctx context.Context, olderThan time.Duration) (int64, error)
	SetRetryBackoffBase(d time.Duration)

	GetSchemaInfo(ctx context.Context) (version int, tableCount int, err error)

	Close() error
}
