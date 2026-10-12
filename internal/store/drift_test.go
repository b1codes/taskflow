package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func TestSQLiteStore_CommandCheckRoundTrip(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	newSessionFixture(t, st, "s1")
	now := time.Now()
	o := &session.Obligation{ID: "o1", SessionID: "s1", RuleID: "regression-suite", RequirementID: "suite", Subject: "web",
		Severity: "required", Describe: "suite", Status: session.ObligationSatisfied, VerifiedLevels: []string{"command"},
		CommandCheck: &session.CommandCheck{Command: "pnpm test", Root: "web", Passed: true, Output: "ok", RanAt: now},
		FirstSeenAt:  now, UpdatedAt: now}
	if err := st.UpsertObligation(ctx, o); err != nil {
		t.Fatal(err)
	}
	// An upsert without a run keeps the stored one.
	o.CommandCheck = nil
	o.Status = session.ObligationOpen
	if err := st.UpsertObligation(ctx, o); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListObligations(ctx, "s1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	c := list[0].CommandCheck
	if c == nil || c.Command != "pnpm test" || !c.Passed || c.Output != "ok" || list[0].Status != session.ObligationOpen {
		t.Errorf("stored = %+v / %+v", list[0], c)
	}
}

func TestSQLiteStore_DriftReport(t *testing.T) {
	t.Parallel()
	st, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	session := func(id, project string, status session.Status) {
		t.Helper()
		if err := st.CreateSession(ctx, &session.Session{ID: id, TaskID: "t-" + id, ProjectPath: project, Status: status, StartedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	session("done1", "/proj/a", "COMPLETED")
	session("done2", "/proj/a/", "ARCHIVED")
	session("live", "/proj/a", "ACTIVE")
	session("other", "/proj/b", "COMPLETED")
	n := 0
	add := func(sessionID, source, req string, status string, reminded int, evidence string, firstSeen time.Time, inherited string) {
		t.Helper()
		n++
		id := fmt.Sprintf("o%d", n)
		o := &obligationRow{id: id, session: sessionID, source: source, req: req, status: status, reminded: reminded, evidence: evidence, firstSeen: firstSeen, inherited: inherited}
		o.insert(t, st)
	}
	old := now.Add(-60 * 24 * time.Hour)
	add("done1", "testing/playwright", "e2e", "satisfied", 0, "", now, "")
	add("done1", "testing/playwright", "e2e", "satisfied", 2, "", now, "")
	add("done2", "testing/playwright", "e2e", "open", 3, "", now, "")
	add("live", "testing/playwright", "e2e", "open", 1, "", now, "")
	add("done2", "testing/playwright", "e2e", "waived", 0, "Feature-flagged screen, no route yet", now, "")
	add("done2", "testing/playwright", "e2e", "attested", 0, "spec e2e/checkout.spec.ts", now, "")
	add("done2", "testing/playwright", "e2e", "void", 0, "", now, "")           // void: left out
	add("done2", "testing/playwright", "e2e", "open", 0, "", now, "o1")         // inherited copy: left out
	add("done1", "framework/react-web", "subject-file", "open", 0, "", now, "") // tf_plan's: left out
	add("done1", "testing/storybook", "story", "satisfied", 0, "", old, "")     // outside --since
	add("other", "testing/playwright", "e2e", "open", 0, "", now, "")           // other project

	rows, err := st.DriftReport(ctx, DriftFilter{ProjectPath: "/proj/a", Since: now.Add(-30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	want := DriftRow{Source: "testing/playwright", RuleID: "rule", RequirementID: "e2e", Obligations: 6, Sessions: 3,
		SatisfiedFirst: 1, SatisfiedAfterReminder: 1, Attested: 1, Waived: 1, OpenAtStop: 1, OpenInProgress: 1,
		WaiverRationales: []string{"Feature-flagged screen, no route yet"}}
	if r.Source != want.Source || r.RuleID != want.RuleID || r.Obligations != want.Obligations || r.Sessions != want.Sessions ||
		r.SatisfiedFirst != 1 || r.SatisfiedAfterReminder != 1 || r.Attested != 1 || r.Waived != 1 || r.OpenAtStop != 1 ||
		r.OpenInProgress != 1 || !slices.Equal(r.WaiverRationales, want.WaiverRationales) {
		t.Errorf("row = %+v\nwant %+v", *r, want)
	}

	// Unfiltered: every project and all time, but still no subject-file rows.
	rows, err = st.DriftReport(ctx, DriftFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("unfiltered rows = %d, %v", len(rows), err)
	}
	if rows[0].Source != "testing/playwright" || rows[0].Obligations != 7 || rows[1].Source != "testing/storybook" {
		t.Errorf("rows = %+v, %+v", *rows[0], *rows[1])
	}
}

type obligationRow struct {
	id, session, source, req, status, evidence, inherited string
	reminded                                              int
	firstSeen                                             time.Time
}

func (o *obligationRow) insert(t *testing.T, st *SQLiteStore) {
	t.Helper()
	var inherited any
	if o.inherited != "" {
		inherited = o.inherited
	}
	_, err := st.db.Exec(`INSERT INTO obligations (id, session_id, rule_id, requirement_id, subject, source, severity, describe,
		status, evidence, reminded_count, inherited_from, first_seen_at, updated_at)
		VALUES (?, ?, 'rule', ?, ?, ?, 'required', 'd', ?, ?, ?, ?, ?, ?)`,
		o.id, o.session, o.req, o.id, o.source, o.status, o.evidence, o.reminded, inherited, o.firstSeen, o.firstSeen)
	if err != nil {
		t.Fatalf("insert %s: %v", o.id, err)
	}
}
