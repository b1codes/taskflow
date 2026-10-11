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
	if again.ID != "o1" {
		t.Errorf("upsert on conflict should report the stored ID, got %q", again.ID)
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

func TestSQLiteStore_SessionPlanState(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")

	if got, _ := st.GetSession(ctx, "s1"); got.PlanState != session.PlanNone {
		t.Fatalf("a new session's plan state = %q, want none", got.PlanState)
	}
	for _, state := range []session.PlanState{session.PlanSuggested, session.PlanPlanned, session.PlanDeclined} {
		if err := st.SetSessionPlanState(ctx, "s1", state); err != nil {
			t.Fatalf("SetSessionPlanState(%s): %v", state, err)
		}
		if got, err := st.GetSessionByTaskID(ctx, "task-s1"); err != nil || got.PlanState != state {
			t.Errorf("plan state = %q, %v; want %s", got.PlanState, err, state)
		}
	}
}

func TestSQLiteStore_AttestAndWaive(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")
	now := time.Now().Truncate(time.Second)
	for _, id := range []string{"o1", "o2"} {
		err := st.UpsertObligation(ctx, &session.Obligation{
			ID: id, SessionID: "s1", RuleID: "r", RequirementID: id, Subject: "A", Severity: "required", Describe: id,
			Status: session.ObligationOpen, VerifiedLevels: []string{"structural"}, FirstSeenAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	d := &session.Decision{ID: "d1", SessionID: "s1", Topic: "waiver", Decision: "skip", Rationale: "n/a", Status: "rejected_alternative", CreatedAt: now}
	if err := st.CreateDecision(ctx, d); err != nil {
		t.Fatal(err)
	}

	if err := st.AttestObligation(ctx, "o1", "spec covers it", []string{"structural", "attest"}, session.ObligationAttested, now); err != nil {
		t.Fatalf("AttestObligation: %v", err)
	}
	if err := st.WaiveObligation(ctx, "o2", "d1", "doesn't apply", now); err != nil {
		t.Fatalf("WaiveObligation: %v", err)
	}
	// A later evaluation's upsert keeps the evidence and the waiver link.
	if err := st.UpsertObligation(ctx, &session.Obligation{
		ID: "new", SessionID: "s1", RuleID: "r", RequirementID: "o2", Subject: "A", Severity: "required", Describe: "o2 v2",
		Status: session.ObligationWaived, FirstSeenAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListObligations(ctx, "s1")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListObligations = %d, %v", len(list), err)
	}
	attested, waived := list[0], list[1]
	if attested.Status != session.ObligationAttested || attested.Evidence != "spec covers it" || !slices.Equal(attested.VerifiedLevels, []string{"structural", "attest"}) || attested.SatisfiedAt != nil {
		t.Errorf("attested = %+v", attested)
	}
	if waived.Status != session.ObligationWaived || waived.WaiverDecisionID != "d1" || waived.Evidence != "doesn't apply" || waived.Describe != "o2 v2" {
		t.Errorf("waived = %+v", waived)
	}

	// Attesting with a satisfied status records when.
	if err := st.AttestObligation(ctx, "o1", "spec covers it", []string{"structural", "attest"}, session.ObligationSatisfied, now); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListObligations(ctx, "s1")
	if list[0].Status != session.ObligationSatisfied || list[0].SatisfiedAt == nil {
		t.Errorf("attested and satisfied = %+v", list[0])
	}
}

// TestSQLiteStore_AttestClearsWaiver: attesting a waived obligation takes
// the waiver back, so it must no longer link the revoked waiver decision.
func TestSQLiteStore_AttestClearsWaiver(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")
	now := time.Now().Truncate(time.Second)
	if err := st.UpsertObligation(ctx, &session.Obligation{
		ID: "o1", SessionID: "s1", RuleID: "r", RequirementID: "e2e", Subject: "A", Severity: "required", Describe: "e2e",
		Status: session.ObligationOpen, FirstSeenAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDecision(ctx, &session.Decision{ID: "d1", SessionID: "s1", Topic: "waiver", Decision: "skip", Rationale: "n/a", Status: "rejected_alternative", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.WaiveObligation(ctx, "o1", "d1", "doesn't apply", now); err != nil {
		t.Fatal(err)
	}
	if err := st.AttestObligation(ctx, "o1", "e2e/a.spec.ts covers it", []string{"attest"}, session.ObligationAttested, now); err != nil {
		t.Fatalf("AttestObligation: %v", err)
	}
	list, err := st.ListObligations(ctx, "s1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListObligations = %d, %v", len(list), err)
	}
	if o := list[0]; o.Status != session.ObligationAttested || o.WaiverDecisionID != "" || o.Evidence != "e2e/a.spec.ts covers it" {
		t.Errorf("attested after waiver = %+v; want the waiver link cleared", o)
	}
}

func TestSQLiteStore_InheritObligations(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "old")
	newSessionFixture(t, st, "new")
	before := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := st.CreateDecision(ctx, &session.Decision{ID: "d1", SessionID: "old", Topic: "t", Decision: "d", Rationale: "r", Status: "rejected_alternative", CreatedAt: before}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*session.Obligation{
		{ID: "open", Status: session.ObligationOpen, RequirementID: "story", Planned: true},
		{ID: "attested", Status: session.ObligationAttested, RequirementID: "e2e", Evidence: "spec", VerifiedLevels: []string{"attest"}},
		{ID: "waived", Status: session.ObligationWaived, RequirementID: "contract", Evidence: "no services", WaiverDecisionID: "d1"},
		{ID: "satisfied", Status: session.ObligationSatisfied, RequirementID: "unit-test", SatisfiedAt: &before},
		{ID: "void", Status: session.ObligationVoid, RequirementID: "gone"},
	} {
		o.SessionID, o.RuleID, o.Subject, o.Severity, o.Describe = "old", "r", "Page.tsx", "required", o.RequirementID
		o.FirstSeenAt, o.UpdatedAt = before, before
		if err := st.UpsertObligation(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.IncrementObligationReminders(ctx, []string{"open"}); err != nil {
		t.Fatal(err)
	}

	if err := st.InheritObligations(ctx, "old", "new"); err != nil {
		t.Fatalf("InheritObligations: %v", err)
	}
	// Inheriting twice is harmless.
	if err := st.InheritObligations(ctx, "old", "new"); err != nil {
		t.Fatalf("InheritObligations again: %v", err)
	}

	got, err := st.ListObligations(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	byFrom := map[string]*session.Obligation{}
	for _, o := range got {
		byFrom[o.InheritedFrom] = o
		if o.ID == o.InheritedFrom || o.SessionID != "new" || o.RemindedCount != 0 || !o.FirstSeenAt.After(before) {
			t.Errorf("copy identity = %+v", o)
		}
	}
	if len(got) != 3 || byFrom["satisfied"] != nil || byFrom["void"] != nil {
		t.Fatalf("inherited = %+v; want open, attested, waived", got)
	}
	if o := byFrom["open"]; o.Status != session.ObligationOpen || !o.Planned {
		t.Errorf("open copy = %+v", o)
	}
	if o := byFrom["attested"]; o.Status != session.ObligationAttested || o.Evidence != "spec" || !slices.Equal(o.VerifiedLevels, []string{"attest"}) {
		t.Errorf("attested copy = %+v", o)
	}
	if o := byFrom["waived"]; o.Status != session.ObligationWaived || o.WaiverDecisionID != "d1" || o.Evidence != "no services" {
		t.Errorf("waived copy = %+v", o)
	}
	if old, _ := st.ListObligations(ctx, "old"); len(old) != 5 {
		t.Errorf("the source session's obligations changed: %d", len(old))
	}
}
