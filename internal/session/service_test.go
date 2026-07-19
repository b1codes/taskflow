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
}

func newMockStore() *mockStore {
	return &mockStore{
		sessions:    make(map[string]*Session),
		checkpoints: make(map[string][]*Checkpoint),
		snags:       make(map[string][]*Snag),
	}
}

func (m *mockStore) CreateSession(ctx context.Context, s *Session) error {
	m.sessions[s.ID] = s
	return nil
}

func (m *mockStore) GetSession(ctx context.Context, id string) (*Session, error) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return s, nil
}

func (m *mockStore) GetSessionByTaskID(ctx context.Context, taskID string) (*Session, error) {
	for _, s := range m.sessions {
		if s.TaskID == taskID {
			return s, nil
		}
	}
	return nil, nil
}

func (m *mockStore) GetActiveSessionByProject(ctx context.Context, projectPath string) (*Session, error) {
	for _, s := range m.sessions {
		if s.ProjectPath == projectPath && s.Status == StatusActive {
			return s, nil
		}
	}
	return nil, nil
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

type mockClickUp struct{}

func (mc *mockClickUp) GetTask(ctx context.Context, taskID string) (*ClickUpTaskInfo, error) {
	return &ClickUpTaskInfo{
		ID:          taskID,
		Name:        "Mock ClickUp Task",
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
