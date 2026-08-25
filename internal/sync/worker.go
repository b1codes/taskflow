package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	if w.clickup == nil {
		// With no ClickUp client configured, wait for shutdown without spinning
		<-ctx.Done()
		return ctx.Err()
	}

	// Reset any stale in-flight ops on worker startup
	_, _ = w.store.ResetStaleInFlight(ctx, 5*time.Minute)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Periodically reset any stale in-flight ops
			_, _ = w.store.ResetStaleInFlight(ctx, 5*time.Minute)

			processed, err := w.processBatch(ctx, 10)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[sync] batch processing error: %v\n", err)
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
	if w.clickup == nil {
		return 0, nil
	}

	_, _ = w.store.ResetStaleInFlight(ctx, 5*time.Minute)

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
	if w.clickup == nil {
		return 0, nil
	}

	ops, err := w.store.DequeuePending(ctx, limit)
	if err != nil {
		return 0, err
	}

	if len(ops) < limit {
		retryable, err := w.store.ListRetryable(ctx)
		if err != nil {
			return 0, err
		}
		remaining := limit - len(ops)
		if len(retryable) > remaining {
			retryable = retryable[:remaining]
		}
		ops = append(ops, retryable...)
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
		return nil
	}

	if err := w.store.MarkInFlight(ctx, op.ID); err != nil {
		fmt.Fprintf(os.Stderr, "[sync] failed to mark op %d in-flight: %v\n", op.ID, err)
		return err
	}

	var payload SyncPayload
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		fmt.Fprintf(os.Stderr, "[sync] invalid payload for op %d (task %s): %v\n", op.ID, op.TaskID, err)
		_ = w.store.MarkFailed(context.Background(), op.ID, fmt.Sprintf("invalid payload: %v", err))
		return err
	}

	var opErr error
	switch op.Operation {
	case "POST_COMMENT", "POST_SNAG", "POST_DECISION":
		opErr = w.clickup.PostComment(ctx, op.TaskID, payload.CommentText)
	case "UPDATE_STATUS":
		statusName, err := clickup.ResolveStatus(ctx, w.store, w.clickup, op.TaskID, payload.StatusType)
		if err != nil {
			opErr = err
		} else {
			opErr = w.clickup.UpdateTaskStatus(ctx, op.TaskID, statusName)
		}
	default:
		opErr = fmt.Errorf("unknown operation: %s", op.Operation)
	}

	if opErr != nil {
		fmt.Fprintf(os.Stderr, "[sync] op %d failed (op: %s, task: %s, retry: %d/%d): %v\n",
			op.ID, op.Operation, op.TaskID, op.Retries+1, op.MaxRetries, opErr)

		var rateLimitErr *clickup.RateLimitError
		if errors.As(opErr, &rateLimitErr) || errors.Is(opErr, clickup.ErrRateLimited) {
			// A 429 response does NOT consume a retry.
			_ = w.store.RequeueRateLimited(context.Background(), op.ID, opErr.Error())
			if rateLimitErr != nil && rateLimitErr.RetryAfter > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(rateLimitErr.RetryAfter):
				}
			}
			return opErr
		}

		_ = w.store.MarkFailed(context.Background(), op.ID, opErr.Error())
		return opErr
	}

	return w.store.MarkDone(ctx, op.ID)
}
