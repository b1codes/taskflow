package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func TestSQLiteStore_Init(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create in-memory store: %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatalf("failed to query sqlite_master: %v", err)
	}
	defer rows.Close()

	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("failed to scan table name: %v", err)
		}
		tables[name] = true
	}

	requiredTables := []string{"schema_version", "sessions", "checkpoints", "snags", "decisions", "sync_queue", "clickup_cache"}
	for _, table := range requiredTables {
		if !tables[table] {
			t.Errorf("expected table %s to exist", table)
		}
	}

	var journalMode string
	err = st.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to get journal mode: %v", err)
	}
	if journalMode != "memory" {
		tempFile, err := os.CreateTemp("", "taskflow-db-*.db")
		if err != nil {
			t.Fatalf("failed to create temp file: %v", err)
		}
		tempPath := tempFile.Name()
		tempFile.Close()
		defer os.Remove(tempPath)

		fileStore, err := New(tempPath)
		if err != nil {
			t.Fatalf("failed to create file store: %v", err)
		}
		defer fileStore.Close()

		var fileJournalMode string
		err = fileStore.db.QueryRow("PRAGMA journal_mode").Scan(&fileJournalMode)
		if err != nil {
			t.Fatalf("failed to get journal mode: %v", err)
		}
		if fileJournalMode != "wal" {
			t.Errorf("expected journal mode to be wal, got %s", fileJournalMode)
		}
	}
}

func TestSQLiteStore_SessionCRUD(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:          "sess-123",
		TaskID:      "task-abc",
		TaskName:    "Test Task",
		ProjectPath: "/tmp/project",
		Status:      session.StatusActive,
		GitBranch:   "main",
		StartedAt:   time.Now().Round(time.Second),
		UpdatedAt:   time.Now().Round(time.Second),
	}

	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	got, err := st.GetSession(ctx, "sess-123")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if got.TaskID != sess.TaskID || got.TaskName != sess.TaskName {
		t.Errorf("GetSession returned mismatch: %+v", got)
	}

	gotByTask, err := st.GetSessionByTaskID(ctx, "task-abc")
	if err != nil {
		t.Fatalf("GetSessionByTaskID failed: %v", err)
	}
	if gotByTask == nil || gotByTask.ID != sess.ID {
		t.Errorf("GetSessionByTaskID returned mismatch: %v", gotByTask)
	}

	gotActive, err := st.GetActiveSessionByProject(ctx, "/tmp/project")
	if err != nil {
		t.Fatalf("GetActiveSessionByProject failed: %v", err)
	}
	if gotActive == nil || gotActive.ID != sess.ID {
		t.Errorf("GetActiveSessionByProject returned mismatch")
	}

	if err := st.UpdateSessionStatus(ctx, "sess-123", session.StatusPaused); err != nil {
		t.Fatalf("UpdateSessionStatus failed: %v", err)
	}
	got, _ = st.GetSession(ctx, "sess-123")
	if got.Status != session.StatusPaused {
		t.Errorf("expected status Paused, got %s", got.Status)
	}

	sessions, err := st.ListSessions(ctx, session.SessionFilter{Status: []session.Status{session.StatusPaused}})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sess.ID {
		t.Errorf("ListSessions returned unexpected sessions: %d", len(sessions))
	}
}

func TestSQLiteStore_CheckpointCRUD(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	// Insert parent session first
	sess := &session.Session{
		ID:          "sess-123",
		TaskID:      "task-abc",
		ProjectPath: "/tmp/project",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	cp := &session.Checkpoint{
		ID:        "cp-123",
		SessionID: "sess-123",
		Summary:   "summary",
		Files:     []string{"a.go", "b.go"},
		GitBranch: "main",
		GitSHA:    "sha123",
		GitDirty:  []string{"c.go"},
		CreatedAt: time.Now().Round(time.Second),
	}

	if err := st.CreateCheckpoint(ctx, cp); err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	checkpoints, err := st.ListCheckpoints(ctx, "sess-123", 5)
	if err != nil {
		t.Fatalf("ListCheckpoints failed: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(checkpoints))
	}
	got := checkpoints[0]
	if got.Summary != cp.Summary || len(got.Files) != 2 || got.Files[0] != "a.go" || got.GitDirty[0] != "c.go" {
		t.Errorf("checkpoint mismatch: %+v", got)
	}
}

func TestSQLiteStore_SnagCRUDAndMatch(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	// Insert parent sessions first
	sess1 := &session.Session{
		ID:          "sess-1",
		TaskID:      "task-1",
		ProjectPath: "/tmp/project1",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess1); err != nil {
		t.Fatalf("CreateSession sess-1 failed: %v", err)
	}

	sess2 := &session.Session{
		ID:          "sess-2",
		TaskID:      "task-2",
		ProjectPath: "/tmp/project2",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess2); err != nil {
		t.Fatalf("CreateSession sess-2 failed: %v", err)
	}

	snag1 := &session.Snag{
		ID:             "snag-1",
		SessionID:      "sess-1",
		Error:          "undefined: foo",
		ErrorSignature: "undefined: foo",
		Category:       "build",
		Resolved:       true,
		CreatedAt:      time.Now(),
		ResolvedAt:     func() *time.Time { t := time.Now(); return &t }(),
		Resolution:     "imported package foo",
	}
	snag2 := &session.Snag{
		ID:             "snag-2",
		SessionID:      "sess-2",
		Error:          "undefined: foo at line 20",
		ErrorSignature: "undefined: foo",
		Category:       "build",
		Resolved:       false,
		CreatedAt:      time.Now(),
	}

	if err := st.CreateSnag(ctx, snag1); err != nil {
		t.Fatalf("CreateSnag failed: %v", err)
	}
	if err := st.CreateSnag(ctx, snag2); err != nil {
		t.Fatalf("CreateSnag failed: %v", err)
	}

	snags, err := st.ListSnagsBySession(ctx, "sess-2")
	if err != nil {
		t.Fatalf("ListSnagsBySession failed: %v", err)
	}
	if len(snags) != 1 || snags[0].ID != snag2.ID {
		t.Errorf("ListSnagsBySession mismatch: %d snags", len(snags))
	}

	resolutions, err := st.FindMatchingResolutions(ctx, "undefined: foo")
	if err != nil {
		t.Fatalf("FindMatchingResolutions failed: %v", err)
	}
	if len(resolutions) != 1 || resolutions[0].ID != snag1.ID {
		t.Errorf("expected to find snag-1 as resolution, got: %d", len(resolutions))
	}
}

func TestSQLiteStore_DecisionCRUD(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:          "sess-dec-123",
		TaskID:      "task-dec",
		ProjectPath: "/tmp/project",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	d := &session.Decision{
		ID:        "dec-123",
		SessionID: "sess-dec-123",
		Topic:     "Folder Naming",
		Decision:  "Retain /internal/gitctx",
		Rationale: "Go idiomatic naming convention",
		Status:    "no_change",
		Files:     []string{"internal/gitctx"},
		CreatedAt: time.Now().Round(time.Second),
	}

	if err := st.CreateDecision(ctx, d); err != nil {
		t.Fatalf("CreateDecision failed: %v", err)
	}

	decisions, err := st.ListDecisionsBySession(ctx, "sess-dec-123")
	if err != nil {
		t.Fatalf("ListDecisionsBySession failed: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	got := decisions[0]
	if got.Topic != d.Topic || got.Decision != d.Decision || got.Rationale != d.Rationale || len(got.Files) != 1 || got.Files[0] != "internal/gitctx" {
		t.Errorf("decision mismatch: %+v", got)
	}
}

// TestPragmas_AppliedPerConnection verifies that foreign_keys and busy_timeout
// are set on every pooled connection, not just one arbitrary connection.
func TestPragmas_AppliedPerConnection(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "pragma_test.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	// Force the pool to open several concurrent connections and verify
	// PRAGMAs on each one independently.
	const numConns = 4

	type pragmaResult struct {
		foreignKeys int
		busyTimeout int
		journalMode string
	}

	results := make([]pragmaResult, numConns)
	var wg sync.WaitGroup

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := st.db.Conn(context.Background())
			if err != nil {
				t.Errorf("Conn failed: %v", err)
				return
			}
			defer conn.Close()

			var r pragmaResult
			_ = conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&r.foreignKeys)
			_ = conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&r.busyTimeout)
			_ = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&r.journalMode)
			results[idx] = r
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.foreignKeys != 1 {
			t.Errorf("conn %d: expected foreign_keys=1, got %d", i, r.foreignKeys)
		}
		if r.busyTimeout != 5000 {
			t.Errorf("conn %d: expected busy_timeout=5000, got %d", i, r.busyTimeout)
		}
		if r.journalMode != "wal" {
			t.Errorf("conn %d: expected journal_mode=wal, got %s", i, r.journalMode)
		}
	}
}

// TestConcurrentWriters_BusyTimeout opens two store handles on the same DB file
// (mirroring `taskflow serve` + a CLI command) and runs concurrent writes.
// With busy_timeout=0 (the old bug), most of these would fail with SQLITE_BUSY.
func TestConcurrentWriters_BusyTimeout(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "concurrent_test.db")

	st1, err := New(dbPath)
	if err != nil {
		t.Fatalf("New(1) failed: %v", err)
	}
	defer st1.Close()

	st2, err := New(dbPath)
	if err != nil {
		t.Fatalf("New(2) failed: %v", err)
	}
	defer st2.Close()

	ctx := context.Background()

	// Create a session to satisfy FK constraints.
	sess := &session.Session{
		ID:          "sess-concurrent",
		TaskID:      "task-concurrent",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st1.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	const writesPerHandle = 20
	errs := make(chan error, writesPerHandle*2)
	var wg sync.WaitGroup

	writer := func(st *SQLiteStore, prefix string) {
		defer wg.Done()
		for i := 0; i < writesPerHandle; i++ {
			cp := &session.Checkpoint{
				ID:        fmt.Sprintf("%s-%d", prefix, i),
				SessionID: "sess-concurrent",
				Summary:   fmt.Sprintf("checkpoint %s-%d", prefix, i),
				CreatedAt: time.Now(),
			}
			if err := st.CreateCheckpoint(ctx, cp); err != nil {
				errs <- err
			}
		}
	}

	wg.Add(2)
	go writer(st1, "h1")
	go writer(st2, "h2")
	wg.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		t.Errorf("concurrent writes: %d/%d failed (expected 0); first error: %v",
			len(failures), writesPerHandle*2, failures[0])
	}

	// Verify all rows were written.
	checkpoints, err := st1.ListCheckpoints(ctx, "sess-concurrent", 0)
	if err != nil {
		t.Fatalf("ListCheckpoints failed: %v", err)
	}
	if len(checkpoints) != writesPerHandle*2 {
		t.Errorf("expected %d checkpoints, got %d", writesPerHandle*2, len(checkpoints))
	}
}

// TestForeignKeys_RejectOrphan verifies that inserting a checkpoint referencing
// a non-existent session is rejected by the FK constraint.
func TestForeignKeys_RejectOrphan(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "fk_test.db")
	st, err := New(dbPath)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Attempt to insert a checkpoint referencing a session that does not exist.
	cp := &session.Checkpoint{
		ID:        "orphan-cp",
		SessionID: "NO-SUCH-SESSION",
		Summary:   "should be rejected by FK constraint",
		CreatedAt: time.Now(),
	}
	err = st.CreateCheckpoint(ctx, cp)
	if err == nil {
		t.Fatal("expected foreign key violation error, but insert succeeded")
	}
	t.Logf("FK violation correctly rejected orphan insert: %v", err)
}

// TestMemoryStore_MultipleConnections forces the pool to open several
// connections against a :memory: store and verifies that data written on one
// connection is visible from all others. Without cache=shared (or with a bare
// ":memory:" DSN), each connection gets its own empty database and this fails.
func TestMemoryStore_MultipleConnections(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	// Temporarily allow multiple connections so we can exercise the pool.
	// (New pins :memory: to MaxOpenConns=1; lift it for this test.)
	st.db.SetMaxOpenConns(4)

	ctx := context.Background()

	// Write a session using the store's normal API.
	sess := &session.Session{
		ID:          "sess-multi-conn",
		TaskID:      "task-multi",
		ProjectPath: "/tmp/multi",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Open several independent connections from the pool and read back
	// the session on each one. If shared cache is not enabled, some
	// connections will see "no such table: sessions".
	const numConns = 4
	var wg sync.WaitGroup

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := st.db.Conn(ctx)
			if err != nil {
				t.Errorf("conn %d: failed to get connection: %v", idx, err)
				return
			}
			defer conn.Close()

			var id string
			err = conn.QueryRowContext(ctx, "SELECT id FROM sessions WHERE id = ?", "sess-multi-conn").Scan(&id)
			if err != nil {
				t.Errorf("conn %d: failed to read session: %v", idx, err)
				return
			}
			if id != "sess-multi-conn" {
				t.Errorf("conn %d: expected id 'sess-multi-conn', got %q", idx, id)
			}
		}(i)
	}
	wg.Wait()
}

func TestResetStaleInFlight_ReapsStale(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-reap",
		Payload:    `{"comment_text":"reap test"}`,
		MaxRetries: 5,
		CreatedAt:  time.Now().Add(-20 * time.Minute),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	// Mark in-flight with timestamp 10 minutes ago
	if err := st.MarkInFlight(ctx, opID); err != nil {
		t.Fatalf("MarkInFlight failed: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	_, err = st.db.ExecContext(ctx, "UPDATE sync_queue SET last_attempted_at = ? WHERE id = ?", staleTime, opID)
	if err != nil {
		t.Fatalf("UPDATE last_attempted_at failed: %v", err)
	}

	// Verify it is not in pending
	pending, err := st.DequeuePending(ctx, 10)
	if err != nil {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending items, got %d", len(pending))
	}

	// Run reaper with 5-minute threshold
	rows, err := st.ResetStaleInFlight(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResetStaleInFlight failed: %v", err)
	}
	if rows != 1 {
		t.Errorf("expected 1 row reset, got %d", rows)
	}

	// Verify op is back in pending
	reaped, err := st.DequeuePending(ctx, 10)
	if err != nil {
		t.Fatalf("DequeuePending after reaper failed: %v", err)
	}
	if len(reaped) != 1 || reaped[0].ID != opID {
		t.Errorf("expected reaped op %d in pending, got %v", opID, reaped)
	}
}

func TestResetStaleInFlight_PreservesActiveInFlight(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-active",
		Payload:    `{"comment_text":"active in flight test"}`,
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	// Mark in-flight now (within the 5-minute window)
	if err := st.MarkInFlight(ctx, opID); err != nil {
		t.Fatalf("MarkInFlight failed: %v", err)
	}

	// Run reaper with 5-minute threshold
	rows, err := st.ResetStaleInFlight(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResetStaleInFlight failed: %v", err)
	}
	if rows != 0 {
		t.Errorf("expected 0 rows reset for active in-flight op, got %d", rows)
	}

	// Verify op is still not in pending
	pending, err := st.DequeuePending(ctx, 10)
	if err != nil {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending items, got %d", len(pending))
	}
}

func TestListRetryable_BackoffGate(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-backoff",
		Payload:    `{"comment_text":"backoff test"}`,
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	// Mark op as failed -> retries becomes 1
	if err := st.MarkFailed(ctx, opID, "network error"); err != nil {
		t.Fatalf("MarkFailed failed: %v", err)
	}

	// 1. Immediately after failure, backoff gate (30s for retry 1) should prevent listing
	st.SetRetryBackoffBase(30 * time.Second)
	retryable, err := st.ListRetryable(ctx)
	if err != nil {
		t.Fatalf("ListRetryable failed: %v", err)
	}
	if len(retryable) != 0 {
		t.Errorf("expected 0 retryables within backoff window, got %d", len(retryable))
	}

	// 2. Backdate last_attempted_at by 35 seconds (past the 30s window)
	staleTime := time.Now().Add(-35 * time.Second)
	_, err = st.db.ExecContext(ctx, "UPDATE sync_queue SET last_attempted_at = ? WHERE id = ?", staleTime, opID)
	if err != nil {
		t.Fatalf("UPDATE last_attempted_at failed: %v", err)
	}

	// Now ListRetryable should return the op
	retryable, err = st.ListRetryable(ctx)
	if err != nil {
		t.Fatalf("ListRetryable after backoff window failed: %v", err)
	}
	if len(retryable) != 1 || retryable[0].ID != opID {
		t.Errorf("expected op %d to be listed after backoff elapsed, got %v", opID, retryable)
	}
}

func TestMarkFailedPermanent(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	op := session.SyncOp{
		Operation:  "UPDATE_STATUS",
		TaskID:     "task-permanent",
		Payload:    `{"role":"complete"}`,
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	const errMsg = "no status for role complete on list"
	if err := st.MarkFailedPermanent(ctx, opID, errMsg); err != nil {
		t.Fatalf("MarkFailedPermanent failed: %v", err)
	}

	// Even with a tiny backoff and a backdated attempt, the op must never be retryable.
	st.SetRetryBackoffBase(1 * time.Millisecond)
	staleTime := time.Now().Add(-1 * time.Hour)
	if _, err := st.db.ExecContext(ctx, "UPDATE sync_queue SET last_attempted_at = ? WHERE id = ?", staleTime, opID); err != nil {
		t.Fatalf("UPDATE last_attempted_at failed: %v", err)
	}

	retryable, err := st.ListRetryable(ctx)
	if err != nil {
		t.Fatalf("ListRetryable failed: %v", err)
	}
	if len(retryable) != 0 {
		t.Errorf("expected 0 retryables after permanent failure, got %d", len(retryable))
	}

	var status, storedMsg string
	var retries, maxRetries int
	if err := st.db.QueryRowContext(ctx, "SELECT status, error_msg, retries, max_retries FROM sync_queue WHERE id = ?", opID).Scan(&status, &storedMsg, &retries, &maxRetries); err != nil {
		t.Fatalf("query sync_queue row failed: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected status FAILED, got %q", status)
	}
	if storedMsg != errMsg {
		t.Errorf("expected error_msg %q, got %q", errMsg, storedMsg)
	}
	if retries != maxRetries {
		t.Errorf("expected retries == max_retries (%d), got %d", maxRetries, retries)
	}
}

func TestRequeueRateLimited(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	op := session.SyncOp{
		Operation:  "POST_COMMENT",
		TaskID:     "task-429",
		Payload:    `{"comment_text":"429 test"}`,
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueSync(ctx, op); err != nil {
		t.Fatalf("EnqueueSync failed: %v", err)
	}

	ops, err := st.DequeuePending(ctx, 1)
	if err != nil || len(ops) != 1 {
		t.Fatalf("DequeuePending failed: %v", err)
	}
	opID := ops[0].ID

	if err := st.MarkInFlight(ctx, opID); err != nil {
		t.Fatalf("MarkInFlight failed: %v", err)
	}

	// Requeue due to 429 rate limit
	if err := st.RequeueRateLimited(ctx, opID, "rate limit 429"); err != nil {
		t.Fatalf("RequeueRateLimited failed: %v", err)
	}

	// Verify status is back to PENDING and retries is still 0
	requeued, err := st.DequeuePending(ctx, 1)
	if err != nil || len(requeued) != 1 {
		t.Fatalf("DequeuePending after requeue failed: %v", err)
	}
	if requeued[0].Retries != 0 {
		t.Errorf("expected retries to remain 0 after 429 requeue, got %d", requeued[0].Retries)
	}
}

func TestStore_SessionNotFound_ErrSessionNotFound(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	sess, err := st.GetSession(ctx, "nonexistent-id")
	if sess != nil || !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound for GetSession, got sess=%v, err=%v", sess, err)
	}

	sess, err = st.GetSessionByTaskID(ctx, "nonexistent-task")
	if sess != nil || !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound for GetSessionByTaskID, got sess=%v, err=%v", sess, err)
	}

	sess, err = st.GetActiveSessionByProject(ctx, "/nonexistent/project")
	if sess != nil || !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound for GetActiveSessionByProject, got sess=%v, err=%v", sess, err)
	}
}

func TestStore_UpdateSessionTaskName(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	sess := &session.Session{
		ID:          "sess-name-update",
		TaskID:      "task-name-update",
		TaskName:    "Old Name",
		ProjectPath: "/tmp",
		Status:      session.StatusActive,
		StartedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := st.UpdateSessionTaskName(ctx, sess.ID, "New Name"); err != nil {
		t.Fatalf("UpdateSessionTaskName failed: %v", err)
	}

	got, err := st.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if got.TaskName != "New Name" {
		t.Errorf("expected TaskName 'New Name', got %q", got.TaskName)
	}
}

func TestSQLiteStore_Indexes(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query("SELECT name FROM sqlite_master WHERE type='index'")
	if err != nil {
		t.Fatalf("failed to query indexes: %v", err)
	}
	defer rows.Close()

	indexes := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("failed to scan index name: %v", err)
		}
		indexes[name] = true
	}

	requiredIndexes := []string{
		"idx_sessions_task_id",
		"idx_sessions_status",
		"idx_sessions_project_status",
		"idx_snags_error_signature",
		"idx_sync_queue_status",
		"idx_clickup_cache_entity",
		"idx_clickup_cache_parent",
		"idx_decisions_session_id",
		"idx_checkpoints_session_id",
		"idx_snags_session_id",
	}
	for _, idx := range requiredIndexes {
		if !indexes[idx] {
			t.Errorf("expected index %s to exist", idx)
		}
	}
}

func latestMigrationVersion() int {
	return migrations[len(migrations)-1].version
}

func TestSQLiteStore_MigrationUpgrade(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", buildDSN(":memory:"))
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	// Initialize with schema version 2 and apply migration 1 and 2
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL); INSERT INTO schema_version (version) VALUES (2);`)
	if err != nil {
		t.Fatalf("failed to setup schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version <= 2 {
			if _, err := db.Exec(m.sql); err != nil {
				t.Fatalf("migration %d failed: %v", m.version, err)
			}
		}
	}

	// Now run runMigrations, which should apply migration 3
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations failed on upgrade: %v", err)
	}

	var version int
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("failed to scan version: %v", err)
	}
	if want := latestMigrationVersion(); version != want {
		t.Errorf("expected schema version %d after upgrade, got %d", want, version)
	}

	// Check that indexes from migration 3 are now present
	var count int
	err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_checkpoints_session_id', 'idx_snags_session_id')").Scan(&count)
	if err != nil {
		t.Fatalf("failed to check indexes: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 new indexes, found %d", count)
	}
}

// TestSQLiteStore_ConcurrentMigrations opens multiple store handles simultaneously on a fresh DB file,
// simulating concurrent process starts (e.g. taskflow serve + CLI commands) to ensure migrations
// run safely without race conditions or lock contention failures.
func TestSQLiteStore_ConcurrentMigrations(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "concurrent_migrations.db")

	const numOpeners = 8
	var wg sync.WaitGroup
	errs := make(chan error, numOpeners)
	stores := make([]*SQLiteStore, numOpeners)

	for i := 0; i < numOpeners; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st, err := New(dbPath)
			if err != nil {
				errs <- err
				return
			}
			stores[idx] = st
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent store.New failed: %v", err)
		}
	}

	for _, st := range stores {
		if st != nil {
			st.Close()
		}
	}

	// Verify final schema state
	verifyStore, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to open verify store: %v", err)
	}
	defer verifyStore.Close()

	version, tableCount, err := verifyStore.GetSchemaInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSchemaInfo failed: %v", err)
	}
	if want := latestMigrationVersion(); version != want {
		t.Errorf("expected version %d, got %d", want, version)
	}
	if tableCount < 7 {
		t.Errorf("expected at least 7 tables, got %d", tableCount)
	}
}

// TestSQLiteStore_Migration4_DropsNameKeyedStatusCache verifies that upgrading
// from v3 removes status cache rows keyed by bare status name (which collided
// across lists) while leaving other cache entities intact.
func TestSQLiteStore_Migration4_DropsNameKeyedStatusCache(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", buildDSN(":memory:"))
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL); INSERT INTO schema_version (version) VALUES (3);`); err != nil {
		t.Fatalf("failed to setup schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version <= 3 {
			if _, err := db.Exec(m.sql); err != nil {
				t.Fatalf("migration %d failed: %v", m.version, err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO clickup_cache (entity_type, entity_id, parent_id, name, status_type, fetched_at) VALUES
		('status', 'complete', 'list-a', 'complete', 'done', CURRENT_TIMESTAMP),
		('task', 'task-1', 'list-a', 'Task 1', NULL, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("failed to seed cache: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations failed on upgrade: %v", err)
	}

	var statusRows, taskRows int
	if err := db.QueryRow(`SELECT count(*) FROM clickup_cache WHERE entity_type = 'status'`).Scan(&statusRows); err != nil {
		t.Fatalf("failed to count status rows: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM clickup_cache WHERE entity_type = 'task'`).Scan(&taskRows); err != nil {
		t.Fatalf("failed to count task rows: %v", err)
	}
	if statusRows != 0 {
		t.Errorf("expected name-keyed status rows to be dropped, found %d", statusRows)
	}
	if taskRows != 1 {
		t.Errorf("expected task cache row to survive, found %d", taskRows)
	}
}

func TestReplaceListStatuses(t *testing.T) {
	t.Parallel()

	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	status := func(listID, name, typ string, order int) *CacheEntry {
		return &CacheEntry{EntityType: "status", EntityID: listID + "/" + name, ParentID: listID, Name: name, StatusType: typ, OrderIndex: order, FetchedAt: time.Now()}
	}

	if err := st.ReplaceListStatuses(ctx, "list-1", []*CacheEntry{status("list-1", "to do", "open", 0), status("list-1", "complete", "closed", 1)}); err != nil {
		t.Fatalf("ReplaceListStatuses failed: %v", err)
	}
	if err := st.ReplaceListStatuses(ctx, "list-2", []*CacheEntry{status("list-2", "to do", "open", 0)}); err != nil {
		t.Fatalf("ReplaceListStatuses failed: %v", err)
	}

	// Renaming "complete" drops the old row; list-2 is untouched.
	if err := st.ReplaceListStatuses(ctx, "list-1", []*CacheEntry{status("list-1", "to do", "open", 0), status("list-1", "shipped", "closed", 1)}); err != nil {
		t.Fatalf("ReplaceListStatuses failed: %v", err)
	}
	got, err := st.GetListStatuses(ctx, "list-1")
	if err != nil {
		t.Fatalf("GetListStatuses failed: %v", err)
	}
	if len(got) != 2 || got[0].Name != "to do" || got[1].Name != "shipped" {
		t.Fatalf("expected [to do, shipped], got %+v", got)
	}
	if other, _ := st.GetListStatuses(ctx, "list-2"); len(other) != 1 {
		t.Fatalf("expected list-2 to keep its 1 status, got %d", len(other))
	}

	// A failed replace must leave the previous set intact.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.ReplaceListStatuses(cancelled, "list-1", []*CacheEntry{status("list-1", "only", "open", 0)}); err == nil {
		t.Fatal("expected an error with a cancelled context")
	}
	got, _ = st.GetListStatuses(ctx, "list-1")
	if len(got) != 2 {
		t.Fatalf("expected the previous 2 statuses after a failed replace, got %d", len(got))
	}
}
