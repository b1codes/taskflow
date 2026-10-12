package clickup

import (
	"context"
	"fmt"
)

// Checklist is a task checklist with its items. Only the fields taskflow
// reconciles are decoded.
type Checklist struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Items []ChecklistItem `json:"items"`
}

type ChecklistItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
}

// checklistResponse is the body of every checklist write: the checklist as
// it stands after the write.
type checklistResponse struct {
	Checklist Checklist `json:"checklist"`
}

// GetTaskChecklists returns the task's checklists, read from GET /task/{id}.
func (c *Client) GetTaskChecklists(ctx context.Context, taskID string) ([]Checklist, error) {
	var task struct {
		Checklists []Checklist `json:"checklists"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/task/%s", taskID), nil, &task); err != nil {
		return nil, err
	}
	return task.Checklists, nil
}

// CreateChecklist adds an empty checklist named name to the task.
func (c *Client) CreateChecklist(ctx context.Context, taskID, name string) (*Checklist, error) {
	var resp checklistResponse
	req := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := c.do(ctx, "POST", fmt.Sprintf("/task/%s/checklist", taskID), req, &resp); err != nil {
		return nil, err
	}
	return &resp.Checklist, nil
}

// CreateChecklistItem adds an unresolved item named name and returns the
// checklist with it.
func (c *Client) CreateChecklistItem(ctx context.Context, checklistID, name string) (*Checklist, error) {
	var resp checklistResponse
	req := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := c.do(ctx, "POST", fmt.Sprintf("/checklist/%s/checklist_item", checklistID), req, &resp); err != nil {
		return nil, err
	}
	return &resp.Checklist, nil
}

// UpdateChecklistItem renames an item and sets whether it is resolved.
func (c *Client) UpdateChecklistItem(ctx context.Context, checklistID, itemID, name string, resolved bool) error {
	req := struct {
		Name     string `json:"name"`
		Resolved bool   `json:"resolved"`
	}{Name: name, Resolved: resolved}
	return c.do(ctx, "PUT", fmt.Sprintf("/checklist/%s/checklist_item/%s", checklistID, itemID), req, nil)
}
