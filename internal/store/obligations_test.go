package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func newSessionFixture(t *testing.T, st *SQLiteStore, id string) {
	t.Helper()
	now := time.Now()
	err := st.CreateSession(context.Background(), &session.Session{
		ID: id, TaskID: "task-" + id, ProjectPath: "/p", Status: session.StatusActive, StartedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestSQLiteStore_SessionBaseSHA(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")

	got, err := st.GetSession(ctx, "s1")
	if err != nil || got.BaseSHA != "" {
		t.Fatalf("a new session has no base: %+v, %v", got, err)
	}
	if err := st.SetSessionBaseSHA(ctx, "s1", "abc"); err != nil {
		t.Fatalf("SetSessionBaseSHA: %v", err)
	}
	for name, get := range map[string]func() (*session.Session, error){
		"GetSession":                func() (*session.Session, error) { return st.GetSession(ctx, "s1") },
		"GetSessionByTaskID":        func() (*session.Session, error) { return st.GetSessionByTaskID(ctx, "task-s1") },
		"GetActiveSessionByProject": func() (*session.Session, error) { return st.GetActiveSessionByProject(ctx, "/p") },
		"ListSessions": func() (*session.Session, error) {
			list, err := st.ListSessions(ctx, session.SessionFilter{})
			if err != nil || len(list) == 0 {
				return nil, err
			}
			return list[0], nil
		},
	} {
		if s, err := get(); err != nil || s.BaseSHA != "abc" {
			t.Errorf("%s: base = %q, %v", name, s.BaseSHA, err)
		}
	}
	var planState string
	if err := st.db.QueryRow(`SELECT plan_state FROM sessions WHERE id = 's1'`).Scan(&planState); err != nil || planState != "none" {
		t.Errorf("plan_state = %q, %v; want none", planState, err)
	}
}

func TestSQLiteStore_ObligationCRUD(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")
	newSessionFixture(t, st, "s2")

	first := time.Now().Add(-time.Hour).Truncate(time.Second)
	o := &session.Obligation{
		ID: "o1", SessionID: "s1", RuleID: "react-screen", RequirementID: "story", Subject: "src/Page.tsx",
		Source: "testing/storybook", Severity: "required", Describe: "Stories for Page",
		Expected: []string{"src/Page.stories.tsx"}, Status: session.ObligationOpen,
		FirstSeenAt: first, UpdatedAt: first,
	}
	if err := st.UpsertObligation(ctx, o); err != nil {
		t.Fatalf("UpsertObligation: %v", err)
	}
	if err := st.IncrementObligationReminders(ctx, []string{"o1"}); err != nil {
		t.Fatalf("IncrementObligationReminders: %v", err)
	}

	// Same key, new ID: updates the existing row and keeps its identity,
	// first_seen_at, and reminder count.
	later := time.Now().Truncate(time.Second)
	again := *o
	again.ID = "o1-duplicate"
	again.Describe = "Storybook stories for Page"
	again.Status = session.ObligationSatisfied
	again.VerifiedLevels = []string{"structural"}
	again.SatisfiedAt = &later
	again.FirstSeenAt, again.UpdatedAt = later, later
	if err := st.UpsertObligation(ctx, &again); err != nil {
		t.Fatalf("UpsertObligation (conflict): %v", err)
	}
	// The same key in another session is a separate obligation.
	other := *o
	other.ID, other.SessionID = "o2", "s2"
	if err := st.UpsertObligation(ctx, &other); err != nil {
		t.Fatalf("UpsertObligation (other session): %v", err)
	}

	list, err := st.ListObligations(ctx, "s1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListObligations = %d rows, %v", len(list), err)
	}
	got := list[0]
	if got.ID != "o1" || got.RemindedCount != 1 || !got.FirstSeenAt.Equal(first) {
		t.Errorf("identity not kept on conflict: %+v", got)
	}
	if got.Describe != "Storybook stories for Page" || got.Status != session.ObligationSatisfied ||
		!slices.Equal(got.VerifiedLevels, []string{"structural"}) || !slices.Equal(got.Expected, []string{"src/Page.stories.tsx"}) ||
		got.SatisfiedAt == nil || !got.SatisfiedAt.Equal(later) || got.Source != "testing/storybook" {
		t.Errorf("fields not updated on conflict: %+v", got)
	}
	if got.Planned || got.InheritedFrom != "" || got.WaiverDecisionID != "" || got.Evidence != "" {
		t.Errorf("unset optional columns should read back empty: %+v", got)
	}

	// open clears satisfied_at; void keeps it.
	if err := st.UpdateObligationStatus(ctx, "o1", session.ObligationOpen, later); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}
	list, _ = st.ListObligations(ctx, "s1")
	if list[0].Status != session.ObligationOpen || list[0].SatisfiedAt != nil {
		t.Errorf("after reopen: %+v", list[0])
	}
	if err := st.UpdateObligationStatus(ctx, "o1", session.ObligationSatisfied, later); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}
	if err := st.UpdateObligationStatus(ctx, "o1", session.ObligationVoid, later.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}
	list, _ = st.ListObligations(ctx, "s1")
	if list[0].Status != session.ObligationVoid || list[0].SatisfiedAt == nil || !list[0].UpdatedAt.Equal(later.Add(time.Minute)) {
		t.Errorf("after void: %+v", list[0])
	}

	if err := st.IncrementObligationReminders(ctx, []string{"o1", "o2"}); err != nil {
		t.Fatalf("IncrementObligationReminders: %v", err)
	}
	if err := st.IncrementObligationReminders(ctx, nil); err != nil {
		t.Fatalf("IncrementObligationReminders(nil): %v", err)
	}
	s1, _ := st.ListObligations(ctx, "s1")
	s2, _ := st.ListObligations(ctx, "s2")
	if s1[0].RemindedCount != 2 || s2[0].RemindedCount != 1 {
		t.Errorf("reminders = %d, %d; want 2, 1", s1[0].RemindedCount, s2[0].RemindedCount)
	}
}

func TestSQLiteStore_ObligationForeignKeys(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	now := time.Now()
	err = st.UpsertObligation(context.Background(), &session.Obligation{
		ID: "o", SessionID: "missing", RuleID: "r", RequirementID: "q", Subject: "s",
		Severity: "required", Describe: "d", Status: session.ObligationOpen, FirstSeenAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Error("expected an obligation for an unknown session to be rejected")
	}
}

// TestSQLiteStore_Migration6_Upgrade upgrades a v5 database that already
// holds sessions: existing rows get an empty base and plan state "none".
func TestSQLiteStore_Migration6_Upgrade(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", buildDSN(":memory:"))
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL); INSERT INTO schema_version (version) VALUES (5);`); err != nil {
		t.Fatalf("failed to setup schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version <= 5 {
			if _, err := db.Exec(m.sql); err != nil {
				t.Fatalf("migration %d failed: %v", m.version, err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, task_id, task_name, project_path, status, git_branch, started_at, updated_at)
		VALUES ('old', 't', 'n', '/p', 'PAUSED', 'main', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("failed to seed session: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations failed on upgrade: %v", err)
	}
	var version int
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil || version != 6 {
		t.Fatalf("version = %d, %v; want 6", version, err)
	}
	st := &SQLiteStore{db: db}
	sess, err := st.GetSession(context.Background(), "old")
	if err != nil || sess.BaseSHA != "" || sess.Status != session.StatusPaused {
		t.Errorf("existing session after upgrade = %+v, %v", sess, err)
	}
	var planState string
	if err := db.QueryRow(`SELECT plan_state FROM sessions WHERE id = 'old'`).Scan(&planState); err != nil || planState != "none" {
		t.Errorf("plan_state = %q, %v", planState, err)
	}
	obligations, err := st.ListObligations(context.Background(), "old")
	if err != nil || len(obligations) != 0 {
		t.Errorf("obligations = %v, %v", obligations, err)
	}
}
