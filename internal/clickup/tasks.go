package clickup

import (
	"context"
	"fmt"
)

type Task struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"text_content"`
	Status      TaskStatus `json:"status"`
	List        TaskList   `json:"list"`
}

type TaskStatus struct {
	Status string `json:"status"`
	Type   string `json:"type"`
}

type TaskList struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	var task Task
	path := fmt.Sprintf("/task/%s", taskID)
	err := c.do(ctx, "GET", path, nil, &task)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

type updateStatusRequest struct {
	Status string `json:"status"`
}

func (c *Client) UpdateTaskStatus(ctx context.Context, taskID, statusName string) error {
	path := fmt.Sprintf("/task/%s", taskID)
	req := updateStatusRequest{Status: statusName}
	return c.do(ctx, "PUT", path, req, nil)
}

type postCommentRequest struct {
	CommentText string `json:"comment_text"`
}

func (c *Client) PostComment(ctx context.Context, taskID, commentBody string) error {
	path := fmt.Sprintf("/task/%s/comment", taskID)
	req := postCommentRequest{CommentText: commentBody}
	return c.do(ctx, "POST", path, req, nil)
}
