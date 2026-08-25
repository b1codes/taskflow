package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type mockStore struct {
	sessions    map[string]*Session
	checkpoints map[string][]*Checkpoint
	snags       map[string][]*Snag
	decisions   map[string][]*Decision
}

func newMockStore() *mockStore {
	return &mockStore{
		sessions:    make(map[string]*Session),
		checkpoints: make(map[string][]*Checkpoint),
		snags:       make(map[string][]*Snag),
		decisions:   make(map[string][]*Decision),
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
	return nil
}

type mockGit struct {
	ctx *GitContext
}

func (mg *mockGit) Capture(projectPath string) (*GitContext, error) {
	return mg.ctx, nil
}

type mockClickUp struct {
	taskName string
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
	}, nil
}

func (mc *mockClickUp) GetListStatuses(ctx context.Context, listID string) ([]ClickUpStatusInfo, error) {
	return []ClickUpStatusInfo{
		{Status: "Open", Type: "open", OrderIndex: 0},
		{Status: "In Progress", Type: "active", OrderIndex: 1},
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

	err = srv.Stop(ctx, res.Session.ID, "paused for lunch", "PAUSED", true)
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
	err = srv.Stop(ctx, sessionID, "pausing for the day", "PAUSED", true)
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
	err = srv.Stop(ctx, sessionID, "finished the feature", "COMPLETED", true)
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

	err = srv.Stop(ctx, "nonexistent-id", "summary", "COMPLETED", false)
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

	err = nilSrv.Stop(ctx, "nonexistent-id", "summary", "COMPLETED", false)
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



