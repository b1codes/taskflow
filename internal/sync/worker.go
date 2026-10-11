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
	store     store.Store
	clickup   *clickup.Client
	config    *config.SyncConfig
	statusMap map[clickup.StatusRole]string
}

type SyncPayload struct {
	CommentText string `json:"comment_text,omitempty"`
	// Role is the lifecycle role (start, complete, archive) for UPDATE_STATUS.
	Role string `json:"role,omitempty"`
	// StatusType is the pre-role UPDATE_STATUS payload; still read so ops
	// queued by an older taskflow version resolve after upgrading.
	StatusType string `json:"status_type,omitempty"`
}

func NewWorker(st store.Store, client *clickup.Client, cfg *config.SyncConfig) *Worker {
	return &Worker{
		store:   st,
		clickup: client,
		config:  cfg,
	}
}

// WithStatusMap sets per-role ClickUp status name overrides from config.
func (w *Worker) WithStatusMap(m config.StatusMapConfig) *Worker {
	w.statusMap = map[clickup.StatusRole]string{
		clickup.RoleStart:    m.Start,
		clickup.RoleReopen:   m.Reopen,
		clickup.RoleComplete: m.Complete,
		clickup.RoleReview:   m.Review,
		clickup.RoleArchive:  m.Archive,
	}
	return w
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
			// Pacing between ClickUp requests is handled by the client
			// (WithMinInterval), since one op can make several API calls.
			err := w.processOp(ctx, op)
			if err == nil {
				count++
			}
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
			// Pacing between ClickUp requests is handled by the client
			// (WithMinInterval), since one op can make several API calls.
			err := w.processOp(ctx, op)
			if err == nil {
				count++
			}
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
		_ = w.store.MarkFailedPermanent(context.Background(), op.ID, fmt.Sprintf("invalid payload: %v", err))
		return err
	}

	var opErr error
	switch op.Operation {
	case "POST_COMMENT", "POST_SNAG", "POST_DECISION":
		opErr = w.clickup.PostComment(ctx, op.TaskID, payload.CommentText)
		if errors.Is(opErr, clickup.ErrBadRequest) {
			opErr = fmt.Errorf("%w: %w", errCommentRejected, opErr)
		}
	case "UPDATE_STATUS":
		opErr = w.updateStatus(ctx, op.TaskID, payload)
	default:
		opErr = fmt.Errorf("%w: %s", errUnknownOperation, op.Operation)
	}

	if opErr != nil {
		fmt.Fprintf(os.Stderr, "[sync] op %d failed (op: %s, task: %s, retry: %d/%d): %v\n",
			op.ID, op.Operation, op.TaskID, op.Retries+1, op.MaxRetries, opErr)

		if errors.Is(opErr, clickup.ErrRateLimited) {
			// A 429 response does NOT consume a retry. The client holds back
			// every request sharing it until Retry-After has passed.
			_ = w.store.RequeueRateLimited(context.Background(), op.ID, opErr.Error())
			return opErr
		}

		if isPermanent(opErr) {
			_ = w.store.MarkFailedPermanent(context.Background(), op.ID, opErr.Error())
			return opErr
		}
		_ = w.store.MarkFailed(context.Background(), op.ID, opErr.Error())
		return opErr
	}

	return w.store.MarkDone(ctx, op.ID)
}

var (
	errUnknownOperation = errors.New("unknown operation")
	// errCommentRejected is a 400 to posting the op's comment: the same
	// comment would be rejected again.
	errCommentRejected = errors.New("comment rejected")
)

// isPermanent reports whether an op error will recur on every retry, so the op
// should fail immediately instead of consuming max_retries attempts. Only a
// 400 to the op's own write counts: a 400 from a lookup along the way (e.g.
// fetching the task to resolve a status role) is retried like any other error.
func isPermanent(err error) bool {
	return errors.Is(err, errUnknownOperation) ||
		errors.Is(err, clickup.ErrUnknownStatusRole) ||
		errors.Is(err, clickup.ErrNoMatchingStatus) ||
		errors.Is(err, errCommentRejected) ||
		// ApplyStatusRole already refreshed the status cache and retried.
		errors.Is(err, clickup.ErrStatusRejected)
}

func (w *Worker) updateStatus(ctx context.Context, taskID string, payload SyncPayload) error {
	var role clickup.StatusRole
	var err error
	if payload.Role != "" {
		role, err = clickup.ParseStatusRole(payload.Role)
	} else {
		role, err = clickup.LegacyStatusRole(payload.StatusType, w.latestSessionCompleted(ctx, taskID))
	}
	if err != nil {
		return err
	}

	err = clickup.ApplyStatusRole(ctx, w.store, w.clickup, taskID, role, w.statusMap)
	if errors.Is(err, clickup.ErrSkipStatusChange) {
		fmt.Fprintf(os.Stderr, "[sync] leaving task %s status unchanged: %v\n", taskID, err)
		return nil
	}
	return err
}

// latestSessionCompleted reports whether the task's most recent local session
// is COMPLETED, which tells a legacy "closed" status op queued by Stop (see
// clickup.LegacyStatusRole) from one queued by archiving.
func (w *Worker) latestSessionCompleted(ctx context.Context, taskID string) bool {
	sess, err := w.store.GetSessionByTaskID(ctx, taskID)
	return err == nil && sess != nil && sess.Status == session.StatusCompleted
}
