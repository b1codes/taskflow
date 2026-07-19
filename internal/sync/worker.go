package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

type Worker struct {
	store   store.Store
	clickup *clickup.Client
	config  *config.SyncConfig
}

type SyncPayload struct {
	CommentText string `json:"comment_text,omitempty"`
	StatusType  string `json:"status_type,omitempty"`
}

func NewWorker(st store.Store, client *clickup.Client, cfg *config.SyncConfig) *Worker {
	return &Worker{
		store:   st,
		clickup: client,
		config:  cfg,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processed, err := w.processBatch(ctx, 10)
			if err != nil {
				// Continue loop even on batch processing errors
			}

			if processed == 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}
	}
}

func (w *Worker) DrainOnce(ctx context.Context) (int, error) {
	pending, err := w.store.DequeuePending(ctx, 100)
	if err != nil {
		return 0, fmt.Errorf("failed to dequeue pending items: %w", err)
	}

	retryable, err := w.store.ListRetryable(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to list retryable items: %w", err)
	}

	var allOps []*session.SyncOp
	allOps = append(allOps, pending...)
	allOps = append(allOps, retryable...)

	count := 0
	for _, op := range allOps {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
			err := w.processOp(ctx, op)
			if err == nil {
				count++
			}
			time.Sleep(time.Duration(w.config.RateLimitMS) * time.Millisecond)
		}
	}

	return count, nil
}

func (w *Worker) processBatch(ctx context.Context, limit int) (int, error) {
	ops, err := w.store.DequeuePending(ctx, limit)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, op := range ops {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
			err := w.processOp(ctx, op)
			if err == nil {
				count++
			}
			time.Sleep(time.Duration(w.config.RateLimitMS) * time.Millisecond)
		}
	}

	return count, nil
}

func (w *Worker) processOp(ctx context.Context, op *session.SyncOp) error {
	if w.clickup == nil {
		return w.store.MarkDone(ctx, op.ID)
	}

	if err := w.store.MarkInFlight(ctx, op.ID); err != nil {
		return err
	}

	var payload SyncPayload
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		_ = w.store.MarkFailed(ctx, op.ID, fmt.Sprintf("invalid payload: %v", err))
		return err
	}

	var opErr error
	switch op.Operation {
	case "POST_COMMENT", "POST_SNAG":
		opErr = w.clickup.PostComment(ctx, op.TaskID, payload.CommentText)
	case "UPDATE_STATUS":
		statusName, err := clickup.ResolveStatus(ctx, w.store, w.clickup, op.TaskID, payload.StatusType)
		if err != nil {
			opErr = err
		} else if statusName != "" {
			opErr = w.clickup.UpdateTaskStatus(ctx, op.TaskID, statusName)
		} else {
			opErr = nil
		}
	default:
		opErr = fmt.Errorf("unknown operation: %s", op.Operation)
	}

	if opErr != nil {
		_ = w.store.MarkFailed(ctx, op.ID, opErr.Error())
		return opErr
	}

	return w.store.MarkDone(ctx, op.ID)
}
