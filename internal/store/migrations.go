package store

import (
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
}

func runMigrations(db *sql.DB) error {
	// Create schema_version table if not exists, and get current version.
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	var currentVersion int
	err = db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&currentVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			currentVersion = 0
		} else {
			return fmt.Errorf("failed to get schema version: %w", err)
		}
	}

	for _, m := range migrations {
		if m.version > currentVersion {
			tx, err := db.Begin()
			if err != nil {
				return fmt.Errorf("failed to start migration transaction: %w", err)
			}
			defer tx.Rollback()

			if _, err := tx.Exec(m.sql); err != nil {
				return fmt.Errorf("failed to execute migration %d: %w", m.version, err)
			}

			if currentVersion == 0 {
				_, err = tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, m.version)
			} else {
				_, err = tx.Exec(`UPDATE schema_version SET version = ?`, m.version)
			}
			if err != nil {
				return fmt.Errorf("failed to update schema version: %w", err)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("failed to commit migration transaction: %w", err)
			}
			currentVersion = m.version
		}
	}

	return nil
}
