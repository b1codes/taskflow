package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func TestSQLiteStore_Init(t *testing.T) {
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

	requiredTables := []string{"schema_version", "sessions", "checkpoints", "snags", "sync_queue", "clickup_cache"}
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
