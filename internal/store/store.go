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

	// Sync Queue
	DequeuePending(ctx context.Context, limit int) ([]*session.SyncOp, error)
	MarkInFlight(ctx context.Context, id int64) error
	MarkDone(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, errMsg string) error
	ListRetryable(ctx context.Context) ([]*session.SyncOp, error)

	GetSchemaInfo(ctx context.Context) (version int, tableCount int, err error)

	Close() error
}
