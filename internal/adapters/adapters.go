package adapters

import (
	"context"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/gitctx"
	"github.com/b1codes/taskflow/internal/session"
)

// GitCapturer implements session.GitCapturer using internal/gitctx.
type GitCapturer struct{}

// NewGitCapturer creates a new session.GitCapturer adapter.
func NewGitCapturer() *GitCapturer {
	return &GitCapturer{}
}

// Capture delegates git context extraction to the gitctx package.
func (g *GitCapturer) Capture(projectPath string) (*session.GitContext, error) {
	return gitctx.Capture(projectPath)
}

// ClickUpAdapter implements session.ClickUpClient using internal/clickup.Client.
type ClickUpAdapter struct {
	client *clickup.Client
}

// NewClickUpAdapter wraps a clickup.Client into a session.ClickUpClient adapter.
func NewClickUpAdapter(client *clickup.Client) *ClickUpAdapter {
	if client == nil {
		return nil
	}
	return &ClickUpAdapter{client: client}
}

// GetTask fetches task details from ClickUp and converts them to session.ClickUpTaskInfo.
func (a *ClickUpAdapter) GetTask(ctx context.Context, taskID string) (*session.ClickUpTaskInfo, error) {
	t, err := a.client.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return &session.ClickUpTaskInfo{
		ID:          t.ID,
		Name:        t.Name,
		Description: t.Description,
		ListID:      t.List.ID,
		StatusType:  t.Status.Type,
	}, nil
}

// GetListStatuses fetches list statuses from ClickUp and converts them to session.ClickUpStatusInfo.
func (a *ClickUpAdapter) GetListStatuses(ctx context.Context, listID string) ([]session.ClickUpStatusInfo, error) {
	details, err := a.client.GetListDetails(ctx, listID)
	if err != nil {
		return nil, err
	}
	var res []session.ClickUpStatusInfo
	for _, s := range details.Statuses {
		res = append(res, session.ClickUpStatusInfo{
			Status:     s.Status,
			Type:       s.Type,
			OrderIndex: s.OrderIndex,
		})
	}
	return res, nil
}
