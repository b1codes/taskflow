package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/b1codes/taskflow/internal/completeness"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/google/uuid"
	"modernc.org/sqlite"
)

type SQLiteStore struct {
	db          *sql.DB
	backoffBase time.Duration
}

func (s *SQLiteStore) SetRetryBackoffBase(d time.Duration) {
	s.backoffBase = d
}

func (s *SQLiteStore) getBackoffBase() time.Duration {
	if s.backoffBase <= 0 {
		return 30 * time.Second
	}
	return s.backoffBase
}

func New(dbPath string) (*SQLiteStore, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// In-memory databases use a unique named DB with cache=shared
	// (see buildDSN) so all pooled connections see the same data.
	// Pin to one connection as a belt-and-suspenders safeguard.
	if dbPath == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	if dbPath != ":memory:" {
		if err := enableWAL(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to enable WAL journal mode: %w", err)
		}
	}

	// Run Migrations
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	st := &SQLiteStore{db: db}

	// Reset any stale in-flight ops left behind by prior crashes or unexpected shutdowns
	_, _ = st.ResetStaleInFlight(context.Background(), 5*time.Minute)

	return st, nil
}

// memDBSeq ensures each store.New(":memory:") gets a unique named
// in-memory database, so parallel tests don't share state.
var memDBSeq atomic.Int64

// buildDSN constructs a SQLite URI with PRAGMAs embedded as query parameters.
// modernc.org/sqlite applies _pragma parameters on every new connection,
// unlike db.Exec("PRAGMA ...") which only reaches one pooled connection.
// foreign_keys and busy_timeout are per-connection settings in SQLite.
func buildDSN(dbPath string) string {
	if dbPath == ":memory:" {
		// Use a unique named in-memory DB with cache=shared so all
		// pooled connections see the same tables and data. Each call
		// gets its own name to keep parallel tests isolated.
		// journal_mode=WAL is not applicable to in-memory databases.
		id := memDBSeq.Add(1)
		return fmt.Sprintf("file:memdb_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", id)
	}
	// journal_mode=WAL is set once by enableWAL rather than here: as a DSN
	// pragma every new connection tried to switch a fresh file to WAL, and
	// concurrent switches fail with SQLITE_BUSY without waiting on
	// busy_timeout.
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
}

// sqliteBusy is SQLite's primary SQLITE_BUSY result code; extended codes
// keep it in their low byte.
const sqliteBusy = 5

// walBusyTimeout bounds how long enableWAL retries, matching busy_timeout.
const walBusyTimeout = 5 * time.Second

// enableWAL puts a file database in WAL mode. The mode is stored in the
// file, so one switch covers every connection. Switching needs an exclusive
// lock, and SQLite reports SQLITE_BUSY at once rather than calling the busy
// handler while other connections hold the file (e.g. processes opening a
// new database together), so it is retried here until walBusyTimeout. A
// filesystem without WAL support leaves the old journal mode, with a warning.
func enableWAL(db *sql.DB) error {
	deadline := time.Now().Add(walBusyTimeout)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				// Some filesystems (network mounts) can't do WAL; SQLite
				// then keeps its rollback journal, which still works.
				fmt.Fprintf(os.Stderr, "[store] warning: journal mode is %q, not wal; continuing without WAL\n", mode)
			}
			return nil
		}
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	planState := sess.PlanState
	if planState == "" {
		planState = session.PlanNone
	}
	query := `INSERT INTO sessions (id, task_id, task_name, project_path, status, git_branch, plan_state, started_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, sess.ID, sess.TaskID, sess.TaskName, sess.ProjectPath, string(sess.Status), sess.GitBranch, string(planState), sess.StartedAt, sess.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, COALESCE(base_sha, ''), plan_state, started_at, updated_at FROM sessions WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.BaseSHA, &sess.PlanState, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, session.ErrSessionNotFound
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	sess.Status = session.Status(statusStr)
	return &sess, nil
}

func (s *SQLiteStore) GetSessionByTaskID(ctx context.Context, taskID string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, COALESCE(base_sha, ''), plan_state, started_at, updated_at FROM sessions WHERE task_id = ? ORDER BY started_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, taskID)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.BaseSHA, &sess.PlanState, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, session.ErrSessionNotFound
		}
		return nil, fmt.Errorf("failed to get session by task id: %w", err)
	}
	sess.Status = session.Status(statusStr)
	return &sess, nil
}

func (s *SQLiteStore) GetActiveSessionByProject(ctx context.Context, projectPath string) (*session.Session, error) {
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, COALESCE(base_sha, ''), plan_state, started_at, updated_at FROM sessions WHERE project_path = ? AND status = 'ACTIVE' LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, projectPath)

	var sess session.Session
	var statusStr string
	err := row.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.BaseSHA, &sess.PlanState, &sess.StartedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, session.ErrSessionNotFound
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

func (s *SQLiteStore) UpdateSessionTaskName(ctx context.Context, id string, taskName string) error {
	query := `UPDATE sessions SET task_name = ?, updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, taskName, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update session task name: %w", err)
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
	query := `SELECT id, task_id, task_name, project_path, status, git_branch, COALESCE(base_sha, ''), plan_state, started_at, updated_at FROM sessions`
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
		err := rows.Scan(&sess.ID, &sess.TaskID, &sess.TaskName, &sess.ProjectPath, &statusStr, &sess.GitBranch, &sess.BaseSHA, &sess.PlanState, &sess.StartedAt, &sess.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan session row: %w", err)
		}
		sess.Status = session.Status(statusStr)
		sessions = append(sessions, &sess)
	}

	return sessions, nil
}

func (s *SQLiteStore) SetSessionBaseSHA(ctx context.Context, id string, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET base_sha = ? WHERE id = ?`, sha, id)
	if err != nil {
		return fmt.Errorf("failed to set session base sha: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SetSessionPlanState(ctx context.Context, id string, state session.PlanState) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET plan_state = ? WHERE id = ?`, string(state), id)
	if err != nil {
		return fmt.Errorf("failed to set session plan state: %w", err)
	}
	return nil
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

// Decisions
func (s *SQLiteStore) CreateDecision(ctx context.Context, d *session.Decision) error {
	query := `INSERT INTO decisions (id, session_id, topic, decision, rationale, status, files, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	filesJSON := jsonMarshal(d.Files)
	statusStr := d.Status
	if statusStr == "" {
		statusStr = "no_change"
	}
	_, err := s.db.ExecContext(ctx, query, d.ID, d.SessionID, d.Topic, d.Decision, d.Rationale, statusStr, filesJSON, d.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create decision: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListDecisionsBySession(ctx context.Context, sessionID string) ([]*session.Decision, error) {
	query := `SELECT id, session_id, topic, decision, rationale, status, files, created_at FROM decisions WHERE session_id = ? ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list decisions: %w", err)
	}
	defer rows.Close()

	var decisions []*session.Decision
	for rows.Next() {
		var d session.Decision
		var filesStr string
		err := rows.Scan(&d.ID, &d.SessionID, &d.Topic, &d.Decision, &d.Rationale, &d.Status, &filesStr, &d.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan decision row: %w", err)
		}
		jsonUnmarshal(filesStr, &d.Files)
		decisions = append(decisions, &d)
	}

	return decisions, nil
}

// Obligations
func (s *SQLiteStore) UpsertObligation(ctx context.Context, o *session.Obligation) error {
	query := `INSERT INTO obligations (id, session_id, rule_id, requirement_id, subject, source, severity, describe, expected,
		status, verified_levels, evidence, waiver_decision_id, planned, inherited_from, reminded_count, first_seen_at, satisfied_at, updated_at,
		command_check)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(session_id, rule_id, requirement_id, subject) DO UPDATE SET
		source = excluded.source,
		severity = excluded.severity,
		describe = excluded.describe,
		expected = excluded.expected,
		status = excluded.status,
		verified_levels = excluded.verified_levels,
		satisfied_at = excluded.satisfied_at,
		updated_at = excluded.updated_at,
		command_check = COALESCE(excluded.command_check, obligations.command_check)
	RETURNING id`
	var planned int
	if o.Planned {
		planned = 1
	}
	err := s.db.QueryRowContext(ctx, query, o.ID, o.SessionID, o.RuleID, o.RequirementID, o.Subject, o.Source, o.Severity, o.Describe,
		jsonMarshal(o.Expected), string(o.Status), jsonMarshal(o.VerifiedLevels), o.Evidence, nullString(o.WaiverDecisionID),
		planned, nullString(o.InheritedFrom), o.RemindedCount, o.FirstSeenAt, o.SatisfiedAt, o.UpdatedAt,
		commandCheckJSON(o.CommandCheck)).Scan(&o.ID)
	if err != nil {
		return fmt.Errorf("failed to upsert obligation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListObligations(ctx context.Context, sessionID string) ([]*session.Obligation, error) {
	query := `SELECT id, session_id, rule_id, requirement_id, subject, COALESCE(source, ''), severity, describe, COALESCE(expected, ''),
		status, COALESCE(verified_levels, ''), COALESCE(evidence, ''), COALESCE(waiver_decision_id, ''), planned,
		COALESCE(inherited_from, ''), reminded_count, first_seen_at, satisfied_at, updated_at, COALESCE(command_check, '')
	FROM obligations WHERE session_id = ? ORDER BY first_seen_at ASC, rule_id, subject, requirement_id`
	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list obligations: %w", err)
	}
	defer rows.Close()

	var obligations []*session.Obligation
	for rows.Next() {
		var o session.Obligation
		var expected, levels, status, commandCheck string
		var planned int
		var satisfiedAt sql.NullTime
		err := rows.Scan(&o.ID, &o.SessionID, &o.RuleID, &o.RequirementID, &o.Subject, &o.Source, &o.Severity, &o.Describe, &expected,
			&status, &levels, &o.Evidence, &o.WaiverDecisionID, &planned,
			&o.InheritedFrom, &o.RemindedCount, &o.FirstSeenAt, &satisfiedAt, &o.UpdatedAt, &commandCheck)
		if err != nil {
			return nil, fmt.Errorf("failed to scan obligation row: %w", err)
		}
		o.Status = session.ObligationStatus(status)
		o.Planned = planned != 0
		jsonUnmarshal(expected, &o.Expected)
		jsonUnmarshal(levels, &o.VerifiedLevels)
		if satisfiedAt.Valid {
			o.SatisfiedAt = &satisfiedAt.Time
		}
		if commandCheck != "" {
			o.CommandCheck = &session.CommandCheck{}
			jsonUnmarshal(commandCheck, o.CommandCheck)
		}
		obligations = append(obligations, &o)
	}
	return obligations, rows.Err()
}

// commandCheckJSON stores a command check as JSON; nil stays NULL, which
// UpsertObligation reads as "keep the stored one".
func commandCheckJSON(c *session.CommandCheck) any {
	if c == nil {
		return nil
	}
	return jsonMarshal(c)
}

func (s *SQLiteStore) UpdateObligationStatus(ctx context.Context, id string, status session.ObligationStatus, at time.Time) error {
	query := `UPDATE obligations SET status = ?, updated_at = ?,
		satisfied_at = CASE ? WHEN 'satisfied' THEN ? WHEN 'open' THEN NULL ELSE satisfied_at END
	WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, string(status), at, string(status), at, id)
	if err != nil {
		return fmt.Errorf("failed to update obligation status: %w", err)
	}
	return nil
}

func (s *SQLiteStore) IncrementObligationReminders(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx, `UPDATE obligations SET reminded_count = reminded_count + 1 WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return fmt.Errorf("failed to increment obligation reminders: %w", err)
	}
	return nil
}

func (s *SQLiteStore) AttestObligation(ctx context.Context, id string, evidence string, levels []string, status session.ObligationStatus, at time.Time) error {
	// An attestation takes back any waiver, so the obligation no longer links
	// to the revoked waiver decision.
	query := `UPDATE obligations SET status = ?, evidence = ?, verified_levels = ?, updated_at = ?,
		satisfied_at = CASE ? WHEN 'satisfied' THEN ? ELSE NULL END,
		waiver_decision_id = NULL
	WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, string(status), evidence, jsonMarshal(levels), at, string(status), at, id)
	if err != nil {
		return fmt.Errorf("failed to attest obligation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) WaiveObligation(ctx context.Context, id string, decisionID string, rationale string, at time.Time) error {
	query := `UPDATE obligations SET status = 'waived', waiver_decision_id = ?, evidence = ?, satisfied_at = NULL, updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, nullString(decisionID), rationale, at, id)
	if err != nil {
		return fmt.Errorf("failed to waive obligation: %w", err)
	}
	return nil
}

// InheritObligations copies in one transaction, so a new session never
// starts with part of what it owes. Copies are new rows: the source
// session's obligations stay as they were recorded.
func (s *SQLiteStore) InheritObligations(ctx context.Context, fromSessionID, toSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin inherit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM obligations WHERE session_id = ? AND status IN ('open', 'attested', 'waived')
	ORDER BY first_seen_at ASC, rule_id, subject, requirement_id`, fromSessionID)
	if err != nil {
		return fmt.Errorf("failed to list obligations to inherit: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan obligation to inherit: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to list obligations to inherit: %w", err)
	}

	now := time.Now()
	for _, id := range ids {
		_, err := tx.ExecContext(ctx, `INSERT INTO obligations (id, session_id, rule_id, requirement_id, subject, source, severity, describe, expected,
			status, verified_levels, evidence, waiver_decision_id, planned, inherited_from, reminded_count, first_seen_at, satisfied_at, updated_at)
		SELECT ?, ?, rule_id, requirement_id, subject, source, severity, describe, expected,
			status, verified_levels, evidence, waiver_decision_id, planned, id, 0, ?, NULL, ?
		FROM obligations WHERE id = ?
		ON CONFLICT(session_id, rule_id, requirement_id, subject) DO NOTHING`,
			uuid.New().String(), toSessionID, now, now, id)
		if err != nil {
			return fmt.Errorf("failed to inherit obligation %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit inherited obligations: %w", err)
	}
	return nil
}

// nullString stores "" as NULL, for columns with foreign keys.
func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
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

func (s *SQLiteStore) ReplaceListStatuses(ctx context.Context, listID string, entries []*CacheEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin status replace: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM clickup_cache WHERE entity_type = 'status' AND parent_id = ?`, listID); err != nil {
		return fmt.Errorf("failed to clear cached statuses for list %s: %w", listID, err)
	}
	query := `INSERT INTO clickup_cache (entity_type, entity_id, parent_id, name, status_type, order_index, extra, fetched_at)
	VALUES ('status', ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		parent_id = excluded.parent_id,
		name = excluded.name,
		status_type = excluded.status_type,
		order_index = excluded.order_index,
		extra = excluded.extra,
		fetched_at = excluded.fetched_at`
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, query, e.EntityID, listID, e.Name, e.StatusType, e.OrderIndex, e.Extra, e.FetchedAt); err != nil {
			return fmt.Errorf("failed to cache status %q for list %s: %w", e.Name, listID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit status replace for list %s: %w", listID, err)
	}
	return nil
}

// Sync Queue
func (s *SQLiteStore) CoalesceSyncOp(ctx context.Context, op session.SyncOp) error {
	// One statement, so the worker can't claim the op between finding it and
	// replacing its payload: a claimed op is IN_FLIGHT and no longer matches.
	// created_at moves the op to the back of the queue, as if newly queued,
	// so it still runs after the ops queued before this state (a decision's
	// comment before the checklist showing the waiver).
	query := `UPDATE sync_queue SET payload = ?, created_at = ? WHERE id = (SELECT MAX(id) FROM sync_queue WHERE task_id = ? AND operation = ?)
	AND (status = 'PENDING' OR (status = 'FAILED' AND retries < max_retries))`
	res, err := s.db.ExecContext(ctx, query, op.Payload, op.CreatedAt, op.TaskID, op.Operation)
	if err != nil {
		return fmt.Errorf("failed to coalesce sync operation: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	return s.EnqueueSync(ctx, op)
}

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
	query := `SELECT ` + syncOpColumns + ` FROM sync_queue WHERE status = 'PENDING' ORDER BY created_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to dequeue pending sync items: %w", err)
	}
	defer rows.Close()
	return scanSyncOps(rows)
}

func (s *SQLiteStore) MarkInFlight(ctx context.Context, id int64) (*session.SyncOp, error) {
	query := `UPDATE sync_queue SET status = 'IN_FLIGHT', last_attempted_at = ? WHERE id = ? AND (status = 'PENDING' OR (status = 'FAILED' AND retries < max_retries))
	RETURNING ` + syncOpColumns
	rows, err := s.db.QueryContext(ctx, query, time.Now(), id)
	if err != nil {
		return nil, fmt.Errorf("failed to mark sync item in-flight: %w", err)
	}
	defer rows.Close()
	ops, err := scanSyncOps(rows)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	return ops[0], nil
}

func (s *SQLiteStore) MarkDone(ctx context.Context, id int64, note string) error {
	query := `UPDATE sync_queue SET status = 'DONE', error_msg = COALESCE(NULLIF(?, ''), error_msg) WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, note, id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item done: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkPartial(ctx context.Context, id int64, note string) error {
	query := `UPDATE sync_queue SET status = 'PARTIAL', error_msg = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, note, id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item partial: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkSkipped(ctx context.Context, id int64, reason string) error {
	query := `UPDATE sync_queue SET status = 'SKIPPED', error_msg = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, reason, id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item skipped: %w", err)
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

func (s *SQLiteStore) MarkFailedPermanent(ctx context.Context, id int64, errMsg string) error {
	query := `UPDATE sync_queue SET status = 'FAILED', retries = max_retries, error_msg = ?, last_attempted_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, errMsg, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark sync item permanently failed: %w", err)
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
	query := `SELECT ` + syncOpColumns + ` FROM sync_queue WHERE status = 'FAILED' AND retries < max_retries ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list retryable sync items: %w", err)
	}
	defer rows.Close()
	all, err := scanSyncOps(rows)
	if err != nil {
		return nil, err
	}

	var ops []*session.SyncOp
	now := time.Now()
	for _, op := range all {
		// Exponential backoff gate: (retries * retries * backoffBase)
		if op.LastAttemptedAt != nil && op.Retries > 0 {
			backoff := time.Duration(op.Retries*op.Retries) * s.getBackoffBase()
			if now.Sub(*op.LastAttemptedAt) < backoff {
				continue // Backoff window has not elapsed yet
			}
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (s *SQLiteStore) HasNewerOp(ctx context.Context, op *session.SyncOp) (bool, error) {
	// Only ops that applied their state or still will count. A SKIPPED op
	// applied nothing, and the op it was skipped for may since have failed
	// for good, so counting it could leave no op to apply any state.
	query := `SELECT EXISTS (SELECT 1 FROM sync_queue WHERE task_id = ? AND operation = ? AND id > ?
	AND (status IN ('PENDING', 'IN_FLIGHT', 'DONE', 'PARTIAL') OR (status = 'FAILED' AND retries < max_retries)))`
	var exists bool
	if err := s.db.QueryRowContext(ctx, query, op.TaskID, op.Operation, op.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("failed to check for newer sync items: %w", err)
	}
	return exists, nil
}

const syncOpColumns = `id, operation, task_id, payload, status, retries, max_retries, created_at, last_attempted_at, error_msg`

func scanSyncOps(rows *sql.Rows) ([]*session.SyncOp, error) {
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
	return ops, rows.Err()
}

func (s *SQLiteStore) LatestSyncOp(ctx context.Context, taskID, operation string) (*session.SyncOp, error) {
	ops, err := s.ListSyncOps(ctx, taskID, operation, 0, 1)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	return ops[0], nil
}

func (s *SQLiteStore) ListSyncOps(ctx context.Context, taskID, operation string, beforeID int64, limit int) ([]*session.SyncOp, error) {
	query := `SELECT ` + syncOpColumns + ` FROM sync_queue WHERE task_id = ? AND operation = ? AND (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, taskID, operation, beforeID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync operations: %w", err)
	}
	defer rows.Close()
	return scanSyncOps(rows)
}

func (s *SQLiteStore) RequeueRateLimited(ctx context.Context, id int64, errMsg string) error {
	query := `UPDATE sync_queue SET status = 'PENDING', error_msg = ?, last_attempted_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, errMsg, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to requeue rate limited sync item: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ResetStaleInFlight(ctx context.Context, olderThan time.Duration) (int64, error) {
	threshold := time.Now().Add(-olderThan)
	query := `UPDATE sync_queue SET status = 'PENDING' WHERE status = 'IN_FLIGHT' AND (last_attempted_at IS NULL OR last_attempted_at < ?)`
	res, err := s.db.ExecContext(ctx, query, threshold)
	if err != nil {
		return 0, fmt.Errorf("failed to reset stale in-flight items: %w", err)
	}
	return res.RowsAffected()
}

// DriftReport selects the obligations in SQL and filters by time and
// aggregates in Go: timestamps are stored as text with their zone offset, so
// comparing them in SQL isn't reliable.
func (s *SQLiteStore) DriftReport(ctx context.Context, filter DriftFilter) ([]*DriftRow, error) {
	query := `SELECT COALESCE(o.source, ''), o.rule_id, o.requirement_id, o.session_id, o.status, o.reminded_count,
		COALESCE(o.evidence, ''), o.first_seen_at, s.status, s.project_path
	FROM obligations o JOIN sessions s ON s.id = o.session_id
	WHERE o.status != 'void' AND o.inherited_from IS NULL AND o.requirement_id != ?
	ORDER BY o.first_seen_at`
	rows, err := s.db.QueryContext(ctx, query, completeness.SubjectFileRequirement)
	if err != nil {
		return nil, fmt.Errorf("failed to query obligations for the drift report: %w", err)
	}
	defer rows.Close()

	byKey := map[string]*DriftRow{}
	sessions := map[string]map[string]bool{}
	var out []*DriftRow
	for rows.Next() {
		var source, ruleID, reqID, sessionID, status, evidence, sessStatus, project string
		var reminded int
		var firstSeen time.Time
		if err := rows.Scan(&source, &ruleID, &reqID, &sessionID, &status, &reminded, &evidence, &firstSeen, &sessStatus, &project); err != nil {
			return nil, fmt.Errorf("failed to scan obligation for the drift report: %w", err)
		}
		if !filter.Since.IsZero() && firstSeen.Before(filter.Since) {
			continue
		}
		if filter.ProjectPath != "" && !sameProject(project, filter.ProjectPath) {
			continue
		}
		k := source + "\x00" + ruleID + "\x00" + reqID
		r := byKey[k]
		if r == nil {
			r = &DriftRow{Source: source, RuleID: ruleID, RequirementID: reqID}
			byKey[k] = r
			sessions[k] = map[string]bool{}
			out = append(out, r)
		}
		r.Obligations++
		sessions[k][sessionID] = true
		r.Sessions = len(sessions[k])
		finished := sessStatus == string(session.StatusCompleted) || sessStatus == string(session.StatusArchived)
		switch session.ObligationStatus(status) {
		case session.ObligationSatisfied:
			if reminded == 0 {
				r.SatisfiedFirst++
			} else {
				r.SatisfiedAfterReminder++
			}
		case session.ObligationAttested:
			r.Attested++
		case session.ObligationWaived:
			r.Waived++
			if evidence != "" {
				r.WaiverRationales = append(r.WaiverRationales, evidence)
			}
		case session.ObligationOpen:
			if finished {
				r.OpenAtStop++
			} else {
				r.OpenInProgress++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		return a.RequirementID < b.RequirementID
	})
	return out, nil
}

// sameProject reports whether a session's stored project path is want,
// comparing cleaned paths (want as given, or made absolute).
func sameProject(stored, want string) bool {
	stored = filepath.Clean(stored)
	if stored == filepath.Clean(want) {
		return true
	}
	abs, err := filepath.Abs(want)
	return err == nil && stored == abs
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
