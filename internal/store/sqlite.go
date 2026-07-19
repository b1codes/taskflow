package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/session"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func New(dbPath string) (*SQLiteStore, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Set PRAGMAs
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to set pragma %q: %w", pragma, err)
		}
	}

	// Run Migrations
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func jsonMarshal(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func jsonUnmarshal(s string, v interface{}) {
	if s == "" {
		s = "[]"
	}
	_ = json.Unmarshal([]byte(s), v)
}

// Sessions
func (s *SQLiteStore) CreateSession(ctx context.Context, sess *session.Session) error {
	query := `INSERT INTO sessions (id, task_id, task_name, project_path, status, git_branch, started_at, updated_at) 
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, sess.ID, sess.TaskID, sess.TaskName, sess.ProjectPath, string(sess.Status), sess.GitBranch, sess.StartedAt, sess.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, started_at, updated_at FROM sessions WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found: %s", id)
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	sess.Status = session.Status(statusStr)
	return &sess, nil
}

func (s *SQLiteStore) GetSessionByTaskID(ctx context.Context, taskID string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, started_at, updated_at FROM sessions WHERE task_id = ? ORDER BY started_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, taskID)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Return nil, nil when not found as per Go conventions or start check
		}
		return nil, fmt.Errorf("failed to get session by task id: %w", err)
	}
	sess.Status = session.Status(statusStr)
	return &sess, nil
}

func (s *SQLiteStore) GetActiveSessionByProject(ctx context.Context, projectPath string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, started_at, updated_at FROM sessions WHERE project_path = ? AND status = 'ACTIVE' LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, projectPath)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get active session: %w", err)
	}
	sess.Status = session.Status(statusStr)
	return &sess, nil
}

func (s *SQLiteStore) UpdateSessionStatus(ctx context.Context, id string, status session.Status) error {
	query := `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, string(status), time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update session status: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateSessionGitBranch(ctx context.Context, id string, branch string) error {
	query := `UPDATE sessions SET git_branch = ?, updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, branch, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update session git branch: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateSessionUpdatedAt(ctx context.Context, id string, updatedAt time.Time) error {
	query := `UPDATE sessions SET updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, updatedAt, id)
	if err != nil {
		return fmt.Errorf("failed to update session updated_at: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListSessions(ctx context.Context, filter session.SessionFilter) ([]*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, started_at, updated_at FROM sessions`
	var args []interface{}
	var conds []string

	if len(filter.Status) > 0 {
		var placeholders []string
		for _, st := range filter.Status {
			placeholders = append(placeholders, "?")
			args = append(args, string(st))
		}
		conds = append(conds, "status IN ("+strings.Join(placeholders, ",")+")")
	}

	if filter.ProjectPath != "" {
		conds = append(conds, "project_path = ?")
		args = append(args, filter.ProjectPath)
	}

	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}

	query += " ORDER BY updated_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*session.Session
	for rows.Next() {
		var sess session.Session
		var statusStr string
		err := rows.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.StartedAt, &sess.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan session row: %w", err)
		}
		sess.Status = session.Status(statusStr)
		sessions = append(sessions, &sess)
	}

	return sessions, nil
}

// Checkpoints
func (s *SQLiteStore) CreateCheckpoint(ctx context.Context, cp *session.Checkpoint) error {
	query := `INSERT INTO checkpoints (id, session_id, summary, files, git_branch, git_sha, git_dirty, created_at) 
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	filesJSON := jsonMarshal(cp.Files)
	dirtyJSON := jsonMarshal(cp.GitDirty)
	_, err := s.db.ExecContext(ctx, query, cp.ID, cp.SessionID, cp.Summary, filesJSON, cp.GitBranch, cp.GitSHA, dirtyJSON, cp.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create checkpoint: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]*session.Checkpoint, error) {
	query := `SELECT id, session_id, summary, files, git_branch, git_sha, git_dirty, created_at FROM checkpoints WHERE session_id = ? ORDER BY created_at DESC`
	var args []interface{}
	args = append(args, sessionID)

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints: %w", err)
	}
	defer rows.Close()

	var checkpoints []*session.Checkpoint
	for rows.Next() {
		var cp session.Checkpoint
		var filesStr, dirtyStr string
		err := rows.Scan(&cp.ID, &cp.SessionID, &cp.Summary, &filesStr, &cp.GitBranch, &cp.GitSHA, &dirtyStr, &cp.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan checkpoint row: %w", err)
		}
		jsonUnmarshal(filesStr, &cp.Files)
		jsonUnmarshal(dirtyStr, &cp.GitDirty)
		checkpoints = append(checkpoints, &cp)
	}

	return checkpoints, nil
}

// Snags
func (s *SQLiteStore) CreateSnag(ctx context.Context, snag *session.Snag) error {
	query := `INSERT INTO snags (id, session_id, error, error_signature, category, resolution, resolved, created_at, resolved_at) 
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	var resolvedInt int
	if snag.Resolved {
		resolvedInt = 1
	}
	_, err := s.db.ExecContext(ctx, query, snag.ID, snag.SessionID, snag.Error, snag.ErrorSignature, snag.Category, snag.Resolution, resolvedInt, snag.CreatedAt, snag.ResolvedAt)
	if err != nil {
		return fmt.Errorf("failed to create snag: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListSnagsBySession(ctx context.Context, sessionID string) ([]*session.Snag, error) {
	query := `SELECT id, session_id, error, error_signature, category, resolution, resolved, created_at, resolved_at FROM snags WHERE session_id = ? ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list snags: %w", err)
	}
	defer rows.Close()

	var snags []*session.Snag
	for rows.Next() {
		var snag session.Snag
		var resolvedInt int
		err := rows.Scan(&snag.ID, &snag.SessionID, &snag.Error, &snag.ErrorSignature, &snag.Category, &snag.Resolution, &resolvedInt, &snag.CreatedAt, &snag.ResolvedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan snag row: %w", err)
		}
		snag.Resolved = resolvedInt != 0
		snags = append(snags, &snag)
	}

	return snags, nil
}

func (s *SQLiteStore) FindMatchingResolutions(ctx context.Context, signature string) ([]*session.Snag, error) {
	query := `SELECT id, session_id, error, error_signature, category, resolution, resolved, created_at, resolved_at FROM snags WHERE error_signature = ? AND resolved = 1 ORDER BY resolved_at DESC`
	rows, err := s.db.QueryContext(ctx, query, signature)
	if err != nil {
		return nil, fmt.Errorf("failed to find matching resolutions: %w", err)
	}
	defer rows.Close()

	var snags []*session.Snag
	for rows.Next() {
		var snag session.Snag
		var resolvedInt int
		err := rows.Scan(&snag.ID, &snag.SessionID, &snag.Error, &snag.ErrorSignature, &snag.Category, &snag.Resolution, &resolvedInt, &snag.CreatedAt, &snag.ResolvedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan snag row: %w", err)
		}
		snag.Resolved = resolvedInt != 0
		snags = append(snags, &snag)
	}

	return snags, nil
}

func (s *SQLiteStore) UpdateSnagResolution(ctx context.Context, id string, resolution string, resolvedAt time.Time) error {
	query := `UPDATE snags SET resolution = ?, resolved = 1, resolved_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, resolution, resolvedAt, id)
	if err != nil {
		return fmt.Errorf("failed to update snag resolution: %w", err)
	}
	return nil
}

// ClickUp Cache
func (s *SQLiteStore) UpsertCacheEntry(ctx context.Context, entry *CacheEntry) error {
	query := `INSERT INTO clickup_cache (entity_type, entity_id, parent_id, name, status_type, order_index, extra, fetched_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		parent_id = excluded.parent_id,
		name = excluded.name,
		status_type = excluded.status_type,
		order_index = excluded.order_index,
		extra = excluded.extra,
		fetched_at = excluded.fetched_at`
	_, err := s.db.ExecContext(ctx, query, entry.EntityType, entry.EntityID, entry.ParentID, entry.Name, entry.StatusType, entry.OrderIndex, entry.Extra, entry.FetchedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert cache entry: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetCacheEntries(ctx context.Context, entityType, parentID string) ([]*CacheEntry, error) {
	query := `SELECT id, entity_type, entity_id, parent_id, name, status_type, order_index, extra, fetched_at FROM clickup_cache WHERE entity_type = ? AND parent_id = ?`
	rows, err := s.db.QueryContext(ctx, query, entityType, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cache entries: %w", err)
	}
	defer rows.Close()

	var entries []*CacheEntry
	for rows.Next() {
		var entry CacheEntry
		var parentIDSql sql.NullString
		var statusTypeSql sql.NullString
		var orderIndexSql sql.NullInt64
		var extraSql sql.NullString
		err := rows.Scan(&entry.ID, &entry.EntityType, &entry.EntityID, &parentIDSql, &entry.Name, &statusTypeSql, &orderIndexSql, &extraSql, &entry.FetchedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan cache entry: %w", err)
		}
		if parentIDSql.Valid {
			entry.ParentID = parentIDSql.String
		}
		if statusTypeSql.Valid {
			entry.StatusType = statusTypeSql.String
		}
		if orderIndexSql.Valid {
			entry.OrderIndex = int(orderIndexSql.Int64)
		}
		if extraSql.Valid {
			entry.Extra = extraSql.String
		}
		entries = append(entries, &entry)
	}
	return entries, nil
}

func (s *SQLiteStore) GetCacheEntry(ctx context.Context, entityType, entityID string) (*CacheEntry, error) {
	query := `SELECT id, entity_type, entity_id, parent_id, name, status_type, order_index, extra, fetched_at FROM clickup_cache WHERE entity_type = ? AND entity_id = ?`
	row := s.db.QueryRowContext(ctx, query, entityType, entityID)

	var entry CacheEntry
	var parentIDSql sql.NullString
	var statusTypeSql sql.NullString
	var orderIndexSql sql.NullInt64
	var extraSql sql.NullString
	err := row.Scan(&entry.ID, &entry.EntityType, &entry.EntityID, &parentIDSql, &entry.Name, &statusTypeSql, &orderIndexSql, &extraSql, &entry.FetchedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get cache entry: %w", err)
	}
	if parentIDSql.Valid {
		entry.ParentID = parentIDSql.String
	}
	if statusTypeSql.Valid {
		entry.StatusType = statusTypeSql.String
	}
	if orderIndexSql.Valid {
		entry.OrderIndex = int(orderIndexSql.Int64)
	}
	if extraSql.Valid {
		entry.Extra = extraSql.String
	}
	return &entry, nil
}

func (s *SQLiteStore) GetListStatuses(ctx context.Context, listID string) ([]*CacheEntry, error) {
	query := `SELECT id, entity_type, entity_id, parent_id, name, status_type, order_index, extra, fetched_at FROM clickup_cache WHERE entity_type = 'status' AND parent_id = ? ORDER BY order_index ASC`
	rows, err := s.db.QueryContext(ctx, query, listID)
	if err != nil {
		return nil, fmt.Errorf("failed to get list statuses: %w", err)
	}
	defer rows.Close()

	var entries []*CacheEntry
	for rows.Next() {
		var entry CacheEntry
		var parentIDSql sql.NullString
		var statusTypeSql sql.NullString
		var orderIndexSql sql.NullInt64
		var extraSql sql.NullString
		err := rows.Scan(&entry.ID, &entry.EntityType, &entry.EntityID, &parentIDSql, &entry.Name, &statusTypeSql, &orderIndexSql, &extraSql, &entry.FetchedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan list status entry: %w", err)
		}
		if parentIDSql.Valid {
			entry.ParentID = parentIDSql.String
		}
		if statusTypeSql.Valid {
			entry.StatusType = statusTypeSql.String
		}
		if orderIndexSql.Valid {
			entry.OrderIndex = int(orderIndexSql.Int64)
		}
		if extraSql.Valid {
			entry.Extra = extraSql.String
		}
		entries = append(entries, &entry)
	}
	return entries, nil
}

func (s *SQLiteStore) IsCacheStale(ctx context.Context, entityType, entityID string, ttl time.Duration) (bool, error) {
	var fetchedAt time.Time
	err := s.db.QueryRowContext(ctx, "SELECT fetched_at FROM clickup_cache WHERE entity_type = ? AND entity_id = ?", entityType, entityID).Scan(&fetchedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil
		}
		return false, fmt.Errorf("failed to check cache freshness: %w", err)
	}
	return time.Since(fetchedAt) > ttl, nil
}

func (s *SQLiteStore) ClearCache(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM clickup_cache")
	if err != nil {
		return fmt.Errorf("failed to clear cache: %w", err)
	}
	return nil
}

// Sync Queue
func (s *SQLiteStore) EnqueueSync(ctx context.Context, op session.SyncOp) error {
	query := `INSERT INTO sync_queue (operation, task_id, payload, status, retries, max_retries, created_at) 
	VALUES (?, ?, ?, 'PENDING', 0, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, op.Operation, op.TaskID, op.Payload, op.MaxRetries, op.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to enqueue sync operation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DequeuePending(ctx context.Context, limit int) ([]*session.SyncOp, error) {
	query := `SELECT id, operation, task_id, payload, status, retries, max_retries, created_at, last_attempted_at, error_msg FROM sync_queue WHERE status = 'PENDING' ORDER BY created_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to dequeue pending sync items: %w", err)
	}
	defer rows.Close()

	var ops []*session.SyncOp
	for rows.Next() {
		var op session.SyncOp
		var lastAttemptedAtSql sql.NullTime
		var errorMsgSql sql.NullString
		err := rows.Scan(&op.ID, &op.Operation, &op.TaskID, &op.Payload, &op.Status, &op.Retries, &op.MaxRetries, &op.CreatedAt, &lastAttemptedAtSql, &errorMsgSql)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sync operation: %w", err)
		}
		if lastAttemptedAtSql.Valid {
			op.LastAttemptedAt = &lastAttemptedAtSql.Time
		}
		if errorMsgSql.Valid {
			op.ErrorMsg = errorMsgSql.String
		}
		ops = append(ops, &op)
	}
	return ops, nil
}

func (s *SQLiteStore) MarkInFlight(ctx context.Context, id int64) error {
	query := `UPDATE sync_queue SET status = 'IN_FLIGHT', last_attempted_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item in-flight: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkDone(ctx context.Context, id int64) error {
	query := `UPDATE sync_queue SET status = 'DONE' WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item done: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkFailed(ctx context.Context, id int64, errMsg string) error {
	// Query current retries and max_retries first
	var retries, maxRetries int
	err := s.db.QueryRowContext(ctx, "SELECT retries, max_retries FROM sync_queue WHERE id = ?", id).Scan(&retries, &maxRetries)
	if err != nil {
		return fmt.Errorf("failed to get sync item retries before marking failed: %w", err)
	}

	newRetries := retries + 1
	if strings.Contains(strings.ToLower(errMsg), "unauthorized") || strings.Contains(strings.ToLower(errMsg), "invalid or expired") {
		newRetries = maxRetries
	}
	status := "FAILED"

	query := `UPDATE sync_queue SET status = ?, retries = ?, error_msg = ?, last_attempted_at = ? WHERE id = ?`
	_, err = s.db.ExecContext(ctx, query, status, newRetries, errMsg, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item failed: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CountPendingByTaskID(ctx context.Context, taskID string) (int, error) {
	query := `SELECT COUNT(*) FROM sync_queue WHERE task_id = ? AND status = 'PENDING'`
	var count int
	err := s.db.QueryRowContext(ctx, query, taskID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count pending sync items: %w", err)
	}
	return count, nil
}

func (s *SQLiteStore) ListRetryable(ctx context.Context) ([]*session.SyncOp, error) {
	query := `SELECT id, operation, task_id, payload, status, retries, max_retries, created_at, last_attempted_at, error_msg FROM sync_queue WHERE status = 'FAILED' AND retries < max_retries ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list retryable sync items: %w", err)
	}
	defer rows.Close()

	var ops []*session.SyncOp
	for rows.Next() {
		var op session.SyncOp
		var lastAttemptedAtSql sql.NullTime
		var errorMsgSql sql.NullString
		err := rows.Scan(&op.ID, &op.Operation, &op.TaskID, &op.Payload, &op.Status, &op.Retries, &op.MaxRetries, &op.CreatedAt, &lastAttemptedAtSql, &errorMsgSql)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sync operation: %w", err)
		}
		if lastAttemptedAtSql.Valid {
			op.LastAttemptedAt = &lastAttemptedAtSql.Time
		}
		if errorMsgSql.Valid {
			op.ErrorMsg = errorMsgSql.String
		}
		ops = append(ops, &op)
	}
	return ops, nil
}

func (s *SQLiteStore) GetSchemaInfo(ctx context.Context) (version int, tableCount int, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err != nil {
		if err == sql.ErrNoRows {
			version = 0
		} else {
			return 0, 0, fmt.Errorf("failed to query schema version: %w", err)
		}
	}

	err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query table count: %w", err)
	}

	return version, tableCount, nil
}
