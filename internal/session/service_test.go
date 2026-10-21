package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

type mockStore struct {
	sessions    map[string]*Session
	checkpoints map[string][]*Checkpoint
	snags       map[string][]*Snag
	decisions   map[string][]*Decision
	obligations map[string][]*Obligation
	syncOps     []SyncOp
	// latestErr fails LatestSyncOp.
	latestErr error
	// planStateErr fails SetSessionPlanState.
	planStateErr error
}

func newMockStore() *mockStore {
	return &mockStore{
		sessions:    make(map[string]*Session),
		checkpoints: make(map[string][]*Checkpoint),
		snags:       make(map[string][]*Snag),
		decisions:   make(map[string][]*Decision),
		obligations: make(map[string][]*Obligation),
	}
}

func (m *mockStore) CreateSession(ctx context.Context, s *Session) error {
	m.sessions[s.ID] = s
	return nil
}

func (m *mockStore) GetSession(ctx context.Context, id string) (*Session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func (m *mockStore) GetSessionByTaskID(ctx context.Context, taskID string) (*Session, error) {
	for _, s := range m.sessions {
		if s.TaskID == taskID {
			return s, nil
		}
	}
	return nil, ErrSessionNotFound
}

func (m *mockStore) GetActiveSessionByProject(ctx context.Context, projectPath string) (*Session, error) {
	for _, s := range m.sessions {
		if s.ProjectPath == projectPath && s.Status == StatusActive {
			return s, nil
		}
	}
	return nil, ErrSessionNotFound
}

func (m *mockStore) UpdateSessionStatus(ctx context.Context, id string, status Status) error {
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.Status = status
	return nil
}

func (m *mockStore) UpdateSessionGitBranch(ctx context.Context, id string, branch string) error {
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.GitBranch = branch
	return nil
}

func (m *mockStore) UpdateSessionTaskName(ctx context.Context, id string, taskName string) error {
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.TaskName = taskName
	return nil
}

func (m *mockStore) UpdateSessionUpdatedAt(ctx context.Context, id string, updatedAt time.Time) error {
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.UpdatedAt = updatedAt
	return nil
}

func (m *mockStore) ListSessions(ctx context.Context, filter SessionFilter) ([]*Session, error) {
	var res []*Session
	for _, s := range m.sessions {
		if filter.ProjectPath != "" && s.ProjectPath != filter.ProjectPath {
			continue
		}
		if len(filter.Status) > 0 {
			match := false
			for _, st := range filter.Status {
				if s.Status == st {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		res = append(res, s)
	}
	return res, nil
}

func (m *mockStore) CreateCheckpoint(ctx context.Context, cp *Checkpoint) error {
	m.checkpoints[cp.SessionID] = append(m.checkpoints[cp.SessionID], cp)
	return nil
}

func (m *mockStore) ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]*Checkpoint, error) {
	list := m.checkpoints[sessionID]
	var rev []*Checkpoint
	for i := len(list) - 1; i >= 0; i-- {
		rev = append(rev, list[i])
	}
	if limit > 0 && len(rev) > limit {
		return rev[:limit], nil
	}
	return rev, nil
}

func (m *mockStore) CreateSnag(ctx context.Context, snag *Snag) error {
	m.snags[snag.SessionID] = append(m.snags[snag.SessionID], snag)
	return nil
}

func (m *mockStore) ListSnagsBySession(ctx context.Context, sessionID string) ([]*Snag, error) {
	return m.snags[sessionID], nil
}

func (m *mockStore) FindMatchingResolutions(ctx context.Context, signature string) ([]*Snag, error) {
	var res []*Snag
	for _, list := range m.snags {
		for _, snag := range list {
			if snag.ErrorSignature == signature && snag.Resolved {
				res = append(res, snag)
			}
		}
	}
	return res, nil
}

func (m *mockStore) UpdateSnagResolution(ctx context.Context, id string, resolution string, resolvedAt time.Time) error {
	for _, list := range m.snags {
		for _, snag := range list {
			if snag.ID == id {
				snag.Resolution = resolution
				snag.Resolved = true
				snag.ResolvedAt = &resolvedAt
				return nil
			}
		}
	}
	return errors.New("not found")
}

func (m *mockStore) CreateDecision(ctx context.Context, d *Decision) error {
	m.decisions[d.SessionID] = append(m.decisions[d.SessionID], d)
	return nil
}

func (m *mockStore) ListDecisionsBySession(ctx context.Context, sessionID string) ([]*Decision, error) {
	return m.decisions[sessionID], nil
}

func (m *mockStore) CountPendingByTaskID(ctx context.Context, taskID string) (int, error) {
	return 0, nil
}

func (m *mockStore) EnqueueSync(ctx context.Context, op SyncOp) error {
	m.syncOps = append(m.syncOps, op)
	return nil
}

// CoalesceSyncOp moves the task's latest op of the same operation to the end
// of the queue with op's payload while a test has marked it PENDING, as the
// SQLite store does for an op still waiting to run. An op with no status is
// taken as synced, so syncOps stays a log of what was queued.
func (m *mockStore) CoalesceSyncOp(ctx context.Context, op SyncOp) error {
	for i := len(m.syncOps) - 1; i >= 0; i-- {
		if have := m.syncOps[i]; have.TaskID == op.TaskID && have.Operation == op.Operation {
			if have.Status == "PENDING" {
				have.Payload = op.Payload
				m.syncOps = append(append(m.syncOps[:i:i], m.syncOps[i+1:]...), have)
				return nil
			}
			break
		}
	}
	return m.EnqueueSync(ctx, op)
}

func (m *mockStore) LatestSyncOp(ctx context.Context, taskID, operation string) (*SyncOp, error) {
	if m.latestErr != nil {
		return nil, m.latestErr
	}
	for i := len(m.syncOps) - 1; i >= 0; i-- {
		if op := m.syncOps[i]; op.TaskID == taskID && op.Operation == operation {
			if op.Status == "" {
				op.Status = "DONE" // see CoalesceSyncOp
			}
			return &op, nil
		}
	}
	return nil, nil
}

func (m *mockStore) SetSessionBaseSHA(ctx context.Context, id string, sha string) error {
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.BaseSHA = sha
	return nil
}

// UpsertObligation and ListObligations copy rows in and out, like the SQLite
// store, so tests can't pass by sharing pointers with the service.
func (m *mockStore) UpsertObligation(ctx context.Context, o *Obligation) error {
	for _, have := range m.obligations[o.SessionID] {
		if have.key() == o.key() {
			have.Source, have.Severity, have.Describe = o.Source, o.Severity, o.Describe
			have.Expected, have.Status, have.VerifiedLevels = o.Expected, o.Status, o.VerifiedLevels
			have.SatisfiedAt, have.UpdatedAt = o.SatisfiedAt, o.UpdatedAt
			if o.CommandCheck != nil {
				have.CommandCheck = o.CommandCheck
			}
			o.ID = have.ID
			return nil
		}
	}
	c := *o
	m.obligations[o.SessionID] = append(m.obligations[o.SessionID], &c)
	return nil
}

func (m *mockStore) ListObligations(ctx context.Context, sessionID string) ([]*Obligation, error) {
	var out []*Obligation
	for _, o := range m.obligations[sessionID] {
		c := *o
		out = append(out, &c)
	}
	return out, nil
}

func (m *mockStore) obligation(id string) *Obligation {
	for _, list := range m.obligations {
		for _, o := range list {
			if o.ID == id {
				return o
			}
		}
	}
	return nil
}

func (m *mockStore) UpdateObligationStatus(ctx context.Context, id string, status ObligationStatus, at time.Time) error {
	o := m.obligation(id)
	if o == nil {
		return errors.New("not found")
	}
	o.Status, o.UpdatedAt = status, at
	switch status {
	case ObligationSatisfied:
		o.SatisfiedAt = &at
	case ObligationOpen:
		o.SatisfiedAt = nil
	}
	return nil
}

func (m *mockStore) IncrementObligationReminders(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if o := m.obligation(id); o != nil {
			o.RemindedCount++
		}
	}
	return nil
}

func (m *mockStore) SetSessionPlanState(ctx context.Context, id string, state PlanState) error {
	if m.planStateErr != nil {
		return m.planStateErr
	}
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("not found")
	}
	s.PlanState = state
	return nil
}

func (m *mockStore) AttestObligation(ctx context.Context, id string, evidence string, levels []string, status ObligationStatus, at time.Time) error {
	o := m.obligation(id)
	if o == nil {
		return errors.New("not found")
	}
	o.Status, o.Evidence, o.VerifiedLevels, o.UpdatedAt = status, evidence, slices.Clone(levels), at
	o.WaiverDecisionID, o.SatisfiedAt = "", nil
	if status == ObligationSatisfied {
		o.SatisfiedAt = &at
	}
	return nil
}

func (m *mockStore) WaiveObligation(ctx context.Context, id string, decisionID string, rationale string, at time.Time) error {
	o := m.obligation(id)
	if o == nil {
		return errors.New("not found")
	}
	o.Status, o.WaiverDecisionID, o.Evidence, o.SatisfiedAt, o.UpdatedAt = ObligationWaived, decisionID, rationale, nil, at
	return nil
}

func (m *mockStore) InheritObligations(ctx context.Context, fromSessionID, toSessionID string) error {
	for _, o := range m.obligations[fromSessionID] {
		switch o.Status {
		case ObligationOpen, ObligationAttested, ObligationWaived:
			c := *o
			c.ID, c.SessionID, c.InheritedFrom = o.ID+"@"+toSessionID, toSessionID, o.ID
			c.RemindedCount, c.SatisfiedAt = 0, nil
			m.obligations[toSessionID] = append(m.obligations[toSessionID], &c)
		}
	}
	return nil
}

// decisionsFor returns the session's logged decisions.
func (m *mockStore) decisionsFor(sessionID string) []*Decision {
	return m.decisions[sessionID]
}

// statusRoles returns the role of every queued UPDATE_STATUS op, in order.
func (m *mockStore) statusRoles(t *testing.T) []StatusRole {
	t.Helper()
	var roles []StatusRole
	for _, op := range m.syncOps {
		if op.Operation != "UPDATE_STATUS" {
			continue
		}
		var payload struct {
			Role StatusRole `json:"role"`
		}
		if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
			t.Fatalf("invalid UPDATE_STATUS payload %q: %v", op.Payload, err)
		}
		roles = append(roles, payload.Role)
	}
	return roles
}

type mockGit struct {
	ctx *GitContext
}

func (mg *mockGit) Capture(projectPath string) (*GitContext, error) {
	return mg.ctx, nil
}

type mockClickUp struct {
	taskName string
	// statusType is the type of the task's current ClickUp status; empty
	// means "open".
	statusType string
}

func (mc *mockClickUp) GetTask(ctx context.Context, taskID string) (*ClickUpTaskInfo, error) {
	name := "Mock ClickUp Task"
	if mc.taskName != "" {
		name = mc.taskName
	}
	return &ClickUpTaskInfo{
		ID:          taskID,
		Name:        name,
		Description: "Mock Description",
		ListID:      "list-123",
		StatusType:  cmp.Or(mc.statusType, "open"),
	}, nil
}

func (mc *mockClickUp) GetListStatuses(ctx context.Context, listID string) ([]ClickUpStatusInfo, error) {
	return []ClickUpStatusInfo{
		{Status: "Open", Type: "open", OrderIndex: 0},
		{Status: "In Progress", Type: "custom", OrderIndex: 1},
		{Status: "Closed", Type: "closed", OrderIndex: 2},
	}, nil
}

func TestService_Start(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-srv-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123", DirtyFiles: []string{}}}
	clickup := &mockClickUp{}
	srv := NewService(store, git, clickup)

	ctx := context.Background()

	res, err := srv.Start(ctx, "task-1", tempDir, true)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if res.Session.Status != StatusActive {
		t.Errorf("expected StatusActive, got %s", res.Session.Status)
	}
	if res.GitContext.Branch != "main" {
		t.Errorf("expected git branch main, got %s", res.GitContext.Branch)
	}
	if res.AgenticContract.Task.Name != "Mock ClickUp Task" {
		t.Errorf("expected ClickUp task name, got %s", res.AgenticContract.Task.Name)
	}

	lockID, err := ReadLock(tempDir)
	if err != nil || lockID != res.Session.ID {
		t.Errorf("lock file mismatch: %s (err: %v)", lockID, err)
	}

	_, err = srv.Start(ctx, "task-2", tempDir, true)
	if err == nil {
		t.Errorf("expected error starting conflicting session in same project path")
	}

	_, err = srv.Stop(ctx, res.Session.ID, "paused for lunch", "PAUSED", true, false)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if res.Session.Status != StatusPaused {
		t.Errorf("expected StatusPaused, got %s", res.Session.Status)
	}

	resResume, err := srv.Start(ctx, "task-1", tempDir, true)
	if err != nil {
		t.Fatalf("Start failed to resume: %v", err)
	}
	if resResume.Session.ID != res.Session.ID {
		t.Errorf("expected same session ID on resume, got: %s", resResume.Session.ID)
	}
	if resResume.Session.Status != StatusActive {
		t.Errorf("expected status Active, got %s", resResume.Session.Status)
	}
}

func TestService_SnagAndCheckpoint(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-srv-test2-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123"}}
	srv := NewService(store, git, nil)

	ctx := context.Background()

	res, _ := srv.Start(ctx, "task-1", tempDir, true)

	cp, err := srv.Checkpoint(ctx, res.Session.ID, "checkpoint 1", []string{"a.go"}, true)
	if err != nil {
		t.Fatalf("Checkpoint failed: %v", err)
	}
	if cp.GitSHA != "sha123" {
		t.Errorf("expected git SHA to be sha123, got: %s", cp.GitSHA)
	}

	snagRes, err := srv.LogSnag(ctx, res.Session.ID, "panic in main.go:10", "build", "")
	if err != nil {
		t.Fatalf("LogSnag failed: %v", err)
	}
	if snagRes.Snag.ErrorSignature != "panic in <file>:<line>" {
		t.Errorf("expected normalized signature, got: %s", snagRes.Snag.ErrorSignature)
	}
}

func TestService_Decision(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-srv-test3-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123"}}
	srv := NewService(store, git, nil)

	ctx := context.Background()

	res, _ := srv.Start(ctx, "task-1", tempDir, true)

	dec, err := srv.LogDecision(ctx, res.Session.ID, "Folder Naming", "Retain /internal/gitctx", "Follows Go naming convention", "no_change", []string{"internal/gitctx"})
	if err != nil {
		t.Fatalf("LogDecision failed: %v", err)
	}
	if dec.Topic != "Folder Naming" {
		t.Errorf("expected topic 'Folder Naming', got: %s", dec.Topic)
	}

	contract, err := srv.BuildAgenticContract(ctx, res.Session, git.ctx, false)
	if err != nil {
		t.Fatalf("BuildAgenticContract failed: %v", err)
	}
	if len(contract.History.Decisions) != 1 {
		t.Fatalf("expected 1 decision in history contract, got: %d", len(contract.History.Decisions))
	}
	if contract.History.Decisions[0].Decision != "Retain /internal/gitctx" {
		t.Errorf("expected decision text 'Retain /internal/gitctx', got: %s", contract.History.Decisions[0].Decision)
	}
}

func TestService_StopPausedWithSummary(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-srv-test-paused-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123"}}
	srv := NewService(store, git, nil)

	ctx := context.Background()

	// 1. Start session (ACTIVE)
	res, err := srv.Start(ctx, "task-1", tempDir, true)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	sessionID := res.Session.ID

	// 2. Stop session as PAUSED with summary
	_, err = srv.Stop(ctx, sessionID, "pausing for the day", "PAUSED", true, false)
	if err != nil {
		t.Fatalf("Stop (PAUSED) failed: %v", err)
	}

	pausedSess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if pausedSess.Status != StatusPaused {
		t.Fatalf("expected status PAUSED, got %s", pausedSess.Status)
	}

	// 3. Verify direct Checkpoint on PAUSED session is refused
	_, err = srv.Checkpoint(ctx, sessionID, "mid-pause checkpoint", nil, true)
	if err == nil {
		t.Fatal("expected Checkpoint on PAUSED session to fail, but it succeeded")
	}

	// 4. Stop session as COMPLETED with summary (was previously failing with 'cannot create checkpoint for non-active session')
	_, err = srv.Stop(ctx, sessionID, "finished the feature", "COMPLETED", true, false)
	if err != nil {
		t.Fatalf("Stop (COMPLETED with summary on PAUSED session) failed: %v", err)
	}

	completedSess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("failed to get completed session: %v", err)
	}
	if completedSess.Status != StatusCompleted {
		t.Fatalf("expected status COMPLETED, got %s", completedSess.Status)
	}

	// 5. Verify both checkpoints were recorded
	cps, err := store.ListCheckpoints(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ListCheckpoints failed: %v", err)
	}
	if len(cps) != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", len(cps))
	}
	// ListCheckpoints returns in reverse chronological order
	if cps[0].Summary != "finished the feature" {
		t.Errorf("expected latest checkpoint summary 'finished the feature', got %q", cps[0].Summary)
	}
	if cps[1].Summary != "pausing for the day" {
		t.Errorf("expected first checkpoint summary 'pausing for the day', got %q", cps[1].Summary)
	}
}

// nilReturningStore returns (nil, nil) on GetSession to test defensive nil guards against panic.
type nilReturningStore struct {
	*mockStore
}

func (n *nilReturningStore) GetSession(ctx context.Context, id string) (*Session, error) {
	return nil, nil
}

func TestService_UnknownSession_NoPanic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Test case 1: Store returns ErrSessionNotFound (standard conforming contract)
	store := newMockStore()
	srv := NewService(store, nil, nil)

	_, err := srv.Checkpoint(ctx, "nonexistent-id", "test checkpoint", nil, false)
	if err == nil {
		t.Errorf("expected error on Checkpoint with nonexistent session, got nil")
	}

	_, err = srv.LogSnag(ctx, "nonexistent-id", "some error", "build", "")
	if err == nil {
		t.Errorf("expected error on LogSnag with nonexistent session, got nil")
	}

	_, err = srv.LogDecision(ctx, "nonexistent-id", "topic", "dec", "rat", "", nil)
	if err == nil {
		t.Errorf("expected error on LogDecision with nonexistent session, got nil")
	}

	_, err = srv.Stop(ctx, "nonexistent-id", "summary", "COMPLETED", false, false)
	if err == nil {
		t.Errorf("expected error on Stop with nonexistent session, got nil")
	}

	// Test case 2: Store returns (nil, nil) - verifies defensive nil guards prevent panic
	nilStore := &nilReturningStore{mockStore: store}
	nilSrv := NewService(nilStore, nil, nil)

	_, err = nilSrv.Checkpoint(ctx, "nonexistent-id", "test checkpoint", nil, false)
	if err == nil {
		t.Errorf("expected error on nilSrv.Checkpoint, got nil")
	}

	_, err = nilSrv.LogSnag(ctx, "nonexistent-id", "some error", "build", "")
	if err == nil {
		t.Errorf("expected error on nilSrv.LogSnag, got nil")
	}

	_, err = nilSrv.LogDecision(ctx, "nonexistent-id", "topic", "dec", "rat", "", nil)
	if err == nil {
		t.Errorf("expected error on nilSrv.LogDecision, got nil")
	}

	_, err = nilSrv.Stop(ctx, "nonexistent-id", "summary", "COMPLETED", false, false)
	if err == nil {
		t.Errorf("expected error on nilSrv.Stop, got nil")
	}
}

func TestService_Start_ActiveSession_ReportsResumedAndRefreshesTaskName(t *testing.T) {
	t.Parallel()

	tempDir, err := os.MkdirTemp("", "taskflow-srv-resume-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123"}}
	clickup := &mockClickUp{taskName: "Initial Task Name"}
	srv := NewService(store, git, clickup)

	ctx := context.Background()

	// First Start: should be fresh session (Resumed = false)
	res1, err := srv.Start(ctx, "task-restart", tempDir, true)
	if err != nil {
		t.Fatalf("First Start failed: %v", err)
	}
	if res1.AgenticContract.Resumed {
		t.Errorf("expected first Start to have Resumed: false, got true")
	}
	if res1.Session.TaskName != "Initial Task Name" {
		t.Errorf("expected task name 'Initial Task Name', got %q", res1.Session.TaskName)
	}

	// Task was renamed in ClickUp
	clickup.taskName = "Renamed Task in ClickUp"

	// Second Start on the same active task without stopping
	res2, err := srv.Start(ctx, "task-restart", tempDir, true)
	if err != nil {
		t.Fatalf("Second Start failed: %v", err)
	}

	// Acceptance criteria: Resumed must be true and session ID must match
	if !res2.AgenticContract.Resumed {
		t.Errorf("expected second Start on ACTIVE session to have Resumed: true, got false")
	}
	if res2.Session.ID != res1.Session.ID {
		t.Errorf("expected same session ID %s, got %s", res1.Session.ID, res2.Session.ID)
	}
	if res2.Session.TaskName != "Renamed Task in ClickUp" {
		t.Errorf("expected refreshed task name 'Renamed Task in ClickUp', got %q", res2.Session.TaskName)
	}
	if res2.AgenticContract.Task.Name != "Renamed Task in ClickUp" {
		t.Errorf("expected contract task name 'Renamed Task in ClickUp', got %q", res2.AgenticContract.Task.Name)
	}
}

func TestService_Start_StaleLockFromDeadProcessIgnored(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-dead-lock-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := newMockStore()
	git := &mockGit{ctx: &GitContext{Branch: "main", SHA: "sha123", DirtyFiles: []string{}}}
	clickup := &mockClickUp{}
	srv := NewService(store, git, clickup)
	ctx := context.Background()

	// Create an active session in store for old task
	oldSess := &Session{
		ID:          "old-crashed-session",
		TaskID:      "task-crashed",
		TaskName:    "Crashed Task",
		ProjectPath: tempDir,
		Status:      StatusActive,
		StartedAt:   time.Now().Add(-1 * time.Hour),
		UpdatedAt:   time.Now().Add(-1 * time.Hour),
	}
	_ = store.CreateSession(ctx, oldSess)

	// Write lock file with a dead PID (e.g. 99999999)
	lockDir := tempDir + "/.b1codes"
	_ = os.MkdirAll(lockDir, 0755)
	deadLockData, _ := json.Marshal(LockInfo{
		SessionID: "old-crashed-session",
		PID:       99999999,
		CreatedAt: time.Now().Add(-1 * time.Hour),
	})
	_ = os.WriteFile(lockDir+"/session.lock", deadLockData, 0644)

	// Start on new task in the same project directory should succeed despite the old active session in lock
	res, err := srv.Start(ctx, "task-new", tempDir, true)
	if err != nil {
		t.Fatalf("expected Start to succeed ignoring stale lock from dead PID, got err: %v", err)
	}
	if res.Session.TaskID != "task-new" {
		t.Errorf("expected session taskID to be 'task-new', got %s", res.Session.TaskID)
	}
}

func TestService_StatusRoles(t *testing.T) {
	ctx := context.Background()

	t.Run("start, pause, complete, then archive", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		res, err := srv.Start(ctx, "task-roles", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		id := res.Session.ID
		for _, target := range []string{"PAUSED", "COMPLETED", "ARCHIVED"} {
			if _, err := srv.Stop(ctx, id, "", target, false, false); err != nil {
				t.Fatalf("Stop(%s) failed: %v", target, err)
			}
		}

		// Archiving the completed session must not queue a status change, or the
		// finished task would move to the list's closed ("cancelled") status.
		// Pausing must not queue a status change either, so a manual ClickUp
		// move made during the session (e.g. to "in review") is preserved.
		want := []StatusRole{RoleStart, RoleComplete}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("re-archiving an archived-after-completed session queues nothing more", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		res, err := srv.Start(ctx, "task-rearchive", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		id := res.Session.ID
		for _, target := range []string{"COMPLETED", "ARCHIVED", "ARCHIVED"} {
			if _, err := srv.Stop(ctx, id, "", target, false, false); err != nil {
				t.Fatalf("Stop(%s) failed: %v", target, err)
			}
		}

		want := []StatusRole{RoleStart, RoleComplete}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("re-archiving an abandoned session doesn't re-send archive", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		res, err := srv.Start(ctx, "task-reabandon", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		id := res.Session.ID
		for _, target := range []string{"ARCHIVED", "ARCHIVED"} {
			if _, err := srv.Stop(ctx, id, "", target, false, false); err != nil {
				t.Fatalf("Stop(%s) failed: %v", target, err)
			}
		}

		want := []StatusRole{RoleStart, RoleArchive}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("pause then resume", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)
		dir := t.TempDir()

		res, err := srv.Start(ctx, "task-resume", dir, false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "PAUSED", false, false); err != nil {
			t.Fatalf("Stop(PAUSED) failed: %v", err)
		}
		resumed, err := srv.Start(ctx, "task-resume", dir, false)
		if err != nil {
			t.Fatalf("resume Start failed: %v", err)
		}
		if resumed.Session.ID != res.Session.ID {
			t.Errorf("expected resume to reuse session %s, got %s", res.Session.ID, resumed.Session.ID)
		}

		// The pause queues nothing; the resume's Start still queues RoleStart.
		want := []StatusRole{RoleStart, RoleStart}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("abandoning an active session archives it", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		res, err := srv.Start(ctx, "task-abandon", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "ARCHIVED", false, false); err != nil {
			t.Fatalf("Stop(ARCHIVED) failed: %v", err)
		}

		want := []StatusRole{RoleStart, RoleArchive}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("new task queues start with the exact payload", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		if _, err := srv.Start(ctx, "task-new", t.TempDir(), false); err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		// The sync worker decodes this payload, so its wire format must not drift.
		if len(store.syncOps) != 1 {
			t.Fatalf("expected 1 queued op, got %d", len(store.syncOps))
		}
		if op := store.syncOps[0]; op.Operation != "UPDATE_STATUS" || op.Payload != `{"role":"start"}` {
			t.Errorf("expected UPDATE_STATUS {\"role\":\"start\"}, got %s %s", op.Operation, op.Payload)
		}
	})

	t.Run("resuming an active session queues start", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)
		dir := t.TempDir()

		for range 2 {
			if _, err := srv.Start(ctx, "task-active", dir, false); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
		}

		want := []StatusRole{RoleStart, RoleStart}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	t.Run("starting after a completed session queues reopen", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)
		dir := t.TempDir()

		res, err := srv.Start(ctx, "task-reopen", dir, false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false); err != nil {
			t.Fatalf("Stop(COMPLETED) failed: %v", err)
		}
		reopened, err := srv.Start(ctx, "task-reopen", dir, false)
		if err != nil {
			t.Fatalf("reopen Start failed: %v", err)
		}
		if reopened.Session.ID == res.Session.ID {
			t.Errorf("expected a new session after completion, got reused %s", res.Session.ID)
		}

		// Only reopen may move the task back out of the done status that the
		// completed session's "complete" put it in.
		want := []StatusRole{RoleStart, RoleComplete, RoleReopen}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
		if last := store.syncOps[len(store.syncOps)-1]; last.Payload != `{"role":"reopen"}` {
			t.Errorf("expected reopen payload {\"role\":\"reopen\"}, got %s", last.Payload)
		}
	})

	t.Run("starting after an abandoned session queues reopen", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)
		dir := t.TempDir()

		res, err := srv.Start(ctx, "task-reabandoned", dir, false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "ARCHIVED", false, false); err != nil {
			t.Fatalf("Stop(ARCHIVED) failed: %v", err)
		}
		if _, err := srv.Start(ctx, "task-reabandoned", dir, false); err != nil {
			t.Fatalf("reopen Start failed: %v", err)
		}

		want := []StatusRole{RoleStart, RoleArchive, RoleReopen}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	// With ClickUp, the task's live status decides, not local history: this
	// DB may never have seen the session that finished the task.
	for _, statusType := range []string{"done", "closed"} {
		t.Run("a task "+statusType+" in ClickUp queues reopen without local history", func(t *testing.T) {
			store := newMockStore()
			srv := NewService(store, &mockGit{}, &mockClickUp{statusType: statusType})

			if _, err := srv.Start(ctx, "task-closed-elsewhere", t.TempDir(), false); err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			want := []StatusRole{RoleReopen}
			if got := store.statusRoles(t); !slices.Equal(got, want) {
				t.Errorf("expected roles %v, got %v", want, got)
			}
		})
	}

	t.Run("a task reopened in ClickUp queues start after a completed session", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, &mockClickUp{})
		dir := t.TempDir()

		res, err := srv.Start(ctx, "task-reopened-elsewhere", dir, false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false); err != nil {
			t.Fatalf("Stop(COMPLETED) failed: %v", err)
		}
		if _, err := srv.Start(ctx, "task-reopened-elsewhere", dir, false); err != nil {
			t.Fatalf("second Start failed: %v", err)
		}

		want := []StatusRole{RoleStart, RoleComplete, RoleStart}
		if got := store.statusRoles(t); !slices.Equal(got, want) {
			t.Errorf("expected roles %v, got %v", want, got)
		}
	})

	for _, status := range []string{"IN_REVIEW", "in_review", "review"} {
		t.Run("stopping with "+status+" marks session completed and queues review", func(t *testing.T) {
			store := newMockStore()
			srv := NewService(store, &mockGit{}, nil)

			res, err := srv.Start(ctx, "task-in-review", t.TempDir(), false)
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			stopRes, err := srv.Stop(ctx, res.Session.ID, "", status, false, false)
			if err != nil {
				t.Fatalf("Stop(%s) failed: %v", status, err)
			}
			if stopRes.Status != StatusCompleted {
				t.Errorf("expected session status COMPLETED, got %s", stopRes.Status)
			}
			want := []StatusRole{RoleStart, RoleReview}
			if got := store.statusRoles(t); !slices.Equal(got, want) {
				t.Errorf("expected roles %v, got %v", want, got)
			}
		})
	}

	for _, status := range []string{"ABANDONED", "abandoned", "cancelled", "canceled"} {
		t.Run("stopping with "+status+" archives session and queues archive", func(t *testing.T) {
			store := newMockStore()
			srv := NewService(store, &mockGit{}, nil)

			res, err := srv.Start(ctx, "task-abandoned", t.TempDir(), false)
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			stopRes, err := srv.Stop(ctx, res.Session.ID, "", status, false, false)
			if err != nil {
				t.Fatalf("Stop(%s) failed: %v", status, err)
			}
			if stopRes.Status != StatusArchived {
				t.Errorf("expected session status ARCHIVED, got %s", stopRes.Status)
			}
			want := []StatusRole{RoleStart, RoleArchive}
			if got := store.statusRoles(t); !slices.Equal(got, want) {
				t.Errorf("expected roles %v, got %v", want, got)
			}
		})
	}

	t.Run("StopWithTaskStatus queues target_status override", func(t *testing.T) {
		store := newMockStore()
		srv := NewService(store, &mockGit{}, nil)

		res, err := srv.Start(ctx, "task-target-status", t.TempDir(), false)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if _, err := srv.StopWithTaskStatus(ctx, res.Session.ID, "", "COMPLETED", "testing/qa", false, false); err != nil {
			t.Fatalf("StopWithTaskStatus failed: %v", err)
		}
		var updatePayloads []string
		for _, op := range store.syncOps {
			if op.Operation == "UPDATE_STATUS" {
				updatePayloads = append(updatePayloads, op.Payload)
			}
		}
		want := []string{`{"role":"start"}`, `{"role":"complete","target_status":"testing/qa"}`}
		if !slices.Equal(updatePayloads, want) {
			t.Errorf("expected UPDATE_STATUS payloads %v, got %v", want, updatePayloads)
		}
	})

	// A target only rides on a completing stop: elsewhere it would be dropped
	// (pausing) or move an archived task past the completion gate.
	for _, tc := range []struct {
		name   string
		first  string // stops the session once before the stop under test
		status string
	}{
		{"pausing", "", "PAUSED"},
		{"archiving", "", "ARCHIVED"},
		{"abandoning", "", "abandoned"},
		{"archiving a completed session", "COMPLETED", "ARCHIVED"},
	} {
		t.Run("StopWithTaskStatus refuses a target when "+tc.name, func(t *testing.T) {
			store := newMockStore()
			srv := NewService(store, &mockGit{}, nil)

			res, err := srv.Start(ctx, "task-target-refused", t.TempDir(), false)
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			sid := res.Session.ID
			if tc.first != "" {
				if _, err := srv.Stop(ctx, sid, "", tc.first, false, false); err != nil {
					t.Fatalf("Stop(%s) failed: %v", tc.first, err)
				}
			}
			before := store.sessions[sid].Status
			roles := store.statusRoles(t)

			_, err = srv.StopWithTaskStatus(ctx, sid, "", tc.status, "complete", false, false)
			if !errors.Is(err, ErrTargetTaskStatus) {
				t.Fatalf("expected ErrTargetTaskStatus, got %v", err)
			}
			if got := store.sessions[sid].Status; got != before {
				t.Errorf("session status = %s, want it left %s", got, before)
			}
			if got := store.statusRoles(t); !slices.Equal(got, roles) {
				t.Errorf("roles = %v, want %v (nothing queued)", got, roles)
			}
		})
	}
}
