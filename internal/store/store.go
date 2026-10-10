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
	MarkInFlight(ctx context.Context, id int64) error
	MarkDone(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, errMsg string) error
	// MarkFailedPermanent marks the op FAILED with retries set to max_retries
	// so ListRetryable never returns it, for errors that cannot succeed on
	// retry (e.g. no matching status on the list, or an invalid payload).
	MarkFailedPermanent(ctx context.Context, id int64, errMsg string) error
	RequeueRateLimited(ctx context.Context, id int64, errMsg string) error
	ListRetryable(ctx context.Context) ([]*session.SyncOp, error)
	ResetStaleInFlight(ctx context.Context, olderThan time.Duration) (int64, error)
	SetRetryBackoffBase(d time.Duration)

	GetSchemaInfo(ctx context.Context) (version int, tableCount int, err error)

	Close() error
}
