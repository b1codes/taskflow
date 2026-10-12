package store

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    task_name TEXT,
    project_path TEXT NOT NULL,
    status TEXT NOT NULL,
    git_branch TEXT,
    started_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS checkpoints (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    summary TEXT NOT NULL,
    files TEXT,
    git_branch TEXT,
    git_sha TEXT,
    git_dirty TEXT,
    created_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS snags (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    error TEXT NOT NULL,
    error_signature TEXT NOT NULL,
    category TEXT,
    resolution TEXT,
    resolved INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    resolved_at DATETIME
);

CREATE TABLE IF NOT EXISTS sync_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation TEXT NOT NULL,
    task_id TEXT NOT NULL,
    payload TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING',
    retries INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 5,
    created_at DATETIME NOT NULL,
    last_attempted_at DATETIME,
    error_msg TEXT
);

CREATE TABLE IF NOT EXISTS clickup_cache (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    parent_id TEXT,
    name TEXT NOT NULL,
    status_type TEXT,
    order_index INTEGER,
    extra TEXT,
    fetched_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_task_id ON sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_project_status ON sessions(project_path, status);
CREATE INDEX IF NOT EXISTS idx_snags_error_signature ON snags(error_signature);
CREATE INDEX IF NOT EXISTS idx_sync_queue_status ON sync_queue(status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_clickup_cache_entity ON clickup_cache(entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_clickup_cache_parent ON clickup_cache(entity_type, parent_id);
`,
	},
	{
		version: 2,
		sql: `
CREATE TABLE IF NOT EXISTS decisions (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    topic TEXT NOT NULL,
    decision TEXT NOT NULL,
    rationale TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'no_change',
    files TEXT,
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_decisions_session_id ON decisions(session_id);
`,
	},
	{
		version: 3,
		sql: `
CREATE INDEX IF NOT EXISTS idx_checkpoints_session_id ON checkpoints(session_id);
CREATE INDEX IF NOT EXISTS idx_snags_session_id ON snags(session_id);
`,
	},
	{
		version: 4,
		// Status cache rows are now keyed "<list_id>/<status name>". Rows keyed by
		// bare name collided across lists; drop them and let the cache repopulate.
		sql: `
DELETE FROM clickup_cache WHERE entity_type = 'status';
`,
	},
	{
		version: 5,
		// The task → list cache is gone: status roles fetch the task live to
		// check its current status, which also gives its list.
		sql: `
DELETE FROM clickup_cache WHERE entity_type = 'task';
`,
	},
	{
		version: 6,
		// Completeness: the commit a session's changes are measured from, its
		// plan state (used from Phase 2), and the obligations it owes.
		sql: `
ALTER TABLE sessions ADD COLUMN base_sha TEXT;
ALTER TABLE sessions ADD COLUMN plan_state TEXT NOT NULL DEFAULT 'none';

CREATE TABLE IF NOT EXISTS obligations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    rule_id TEXT NOT NULL,
    requirement_id TEXT NOT NULL,
    subject TEXT NOT NULL,
    source TEXT,
    severity TEXT NOT NULL,
    describe TEXT NOT NULL,
    expected TEXT,
    status TEXT NOT NULL,
    verified_levels TEXT,
    evidence TEXT,
    waiver_decision_id TEXT REFERENCES decisions(id),
    planned INTEGER NOT NULL DEFAULT 0,
    inherited_from TEXT REFERENCES obligations(id),
    reminded_count INTEGER NOT NULL DEFAULT 0,
    first_seen_at DATETIME NOT NULL,
    satisfied_at DATETIME,
    updated_at DATETIME NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_obligations_key ON obligations(session_id, rule_id, requirement_id, subject);
CREATE INDEX IF NOT EXISTS idx_obligations_requirement ON obligations(rule_id, requirement_id);
`,
	},
	{
		version: 7,
		// Full-state ops (SYNC_CHECKLIST) look up the task's other ops of the
		// same operation: HasNewerOp, LatestSyncOp, ListSyncOps.
		sql: `
CREATE INDEX IF NOT EXISTS idx_sync_queue_task_op ON sync_queue(task_id, operation, id);
`,
	},
	{
		version: 8,
		// Completeness Phase 4: the last run of an obligation's command check
		// (JSON session.CommandCheck), kept so a pass counts until the next run.
		sql: `
ALTER TABLE obligations ADD COLUMN command_check TEXT;
`,
	},
}

func runMigrations(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get database connection for migrations: %w", err)
	}
	defer conn.Close()

	// Acquire immediate transaction to guard migrations across concurrent processes
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("failed to begin immediate migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Create schema_version table if not exists, and get current version.
	_, err = conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	var currentVersion int
	err = conn.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`).Scan(&currentVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			currentVersion = 0
		} else {
			return fmt.Errorf("failed to get schema version: %w", err)
		}
	}

	for _, m := range migrations {
		if m.version > currentVersion {
			if _, err := conn.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("failed to execute migration %d: %w", m.version, err)
			}

			if currentVersion == 0 {
				_, err = conn.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, m.version)
			} else {
				_, err = conn.ExecContext(ctx, `UPDATE schema_version SET version = ?`, m.version)
			}
			if err != nil {
				return fmt.Errorf("failed to update schema version: %w", err)
			}

			currentVersion = m.version
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("failed to commit migration transaction: %w", err)
	}
	committed = true

	return nil
}
