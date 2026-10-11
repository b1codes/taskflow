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
	case session.OpSyncChecklist:
		opErr = w.syncChecklist(ctx, op)
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
	// errChecklistRejected is a 400 to one of a SYNC_CHECKLIST op's writes
	// (creating the checklist, or creating or updating an item).
	errChecklistRejected = errors.New("checklist change rejected")
	errInvalidPayload    = errors.New("invalid payload")
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
		errors.Is(err, errChecklistRejected) ||
		errors.Is(err, errInvalidPayload) ||
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

// syncChecklist reconciles the task's Definition of Done checklist with the
// op's payload, the checklist's full desired state: it finds the checklist by
// name (creating it if missing), creates missing items, and renames or
// (un)resolves items that differ. Items are never deleted, and void items
// are only renamed, never created. The op is skipped when a newer one for
// the task will apply a later state anyway.
func (w *Worker) syncChecklist(ctx context.Context, op *session.SyncOp) error {
	var p session.ChecklistPayload
	if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
		return fmt.Errorf("%w: %w", errInvalidPayload, err)
	}
	newer, err := w.store.HasNewerOp(ctx, op)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync] couldn't check for a newer checklist op than %d: %v\n", op.ID, err)
	}
	if newer {
		return nil
	}

	checklists, err := w.clickup.GetTaskChecklists(ctx, op.TaskID)
	if err != nil {
		return fmt.Errorf("fetching checklists: %w", err)
	}
	var cl *clickup.Checklist
	for i := range checklists {
		if checklists[i].Name == p.ChecklistName {
			cl = &checklists[i]
			break
		}
	}
	if cl == nil {
		if cl, err = w.clickup.CreateChecklist(ctx, op.TaskID, p.ChecklistName); err != nil {
			return checklistWriteError("creating checklist", err)
		}
	}

	matched := matchChecklistItems(cl.Items, p.Items)
	for i, it := range p.Items {
		ex, ok := matched[i]
		if !ok {
			if it.Void {
				continue
			}
			known := map[string]bool{}
			for _, ci := range cl.Items {
				known[ci.ID] = true
			}
			updated, err := w.clickup.CreateChecklistItem(ctx, cl.ID, it.Name)
			if err != nil {
				return checklistWriteError(fmt.Sprintf("creating checklist item %q", it.Name), err)
			}
			if len(updated.Items) > 0 {
				cl.Items = updated.Items
			}
			if !it.Resolved {
				continue
			}
			// New items start unresolved; find this one to resolve it.
			for _, ci := range cl.Items {
				if !known[ci.ID] && ci.Name == it.Name {
					ex, ok = ci, true
					break
				}
			}
			if !ok {
				// A retry finds the item by name and resolves it.
				return fmt.Errorf("created checklist item %q is missing from ClickUp's response", it.Name)
			}
		}
		if ex.Name != it.Name || ex.Resolved != it.Resolved {
			if err := w.clickup.UpdateChecklistItem(ctx, cl.ID, ex.ID, it.Name, it.Resolved); err != nil {
				return checklistWriteError(fmt.Sprintf("updating checklist item %q", it.Name), err)
			}
		}
	}
	return nil
}

// matchChecklistItems pairs each wanted item with an existing ClickUp item,
// each existing item used at most once: first by exact name, then by the
// wanted item's stable part (session.ChecklistItemMatches), so an item whose
// status text changed is updated rather than duplicated.
func matchChecklistItems(existing []clickup.ChecklistItem, want []session.ChecklistItem) map[int]clickup.ChecklistItem {
	matched := map[int]clickup.ChecklistItem{}
	used := map[string]bool{}
	pass := func(match func(name string, it session.ChecklistItem) bool) {
		for i, it := range want {
			if _, ok := matched[i]; ok {
				continue
			}
			for _, ci := range existing {
				if !used[ci.ID] && match(ci.Name, it) {
					matched[i], used[ci.ID] = ci, true
					break
				}
			}
		}
	}
	pass(func(name string, it session.ChecklistItem) bool { return name == it.Name })
	pass(func(name string, it session.ChecklistItem) bool { return session.ChecklistItemMatches(name, it.Match) })
	return matched
}

// checklistWriteError wraps a failed checklist write; a 400 is permanent,
// since the same write would be rejected again.
func checklistWriteError(what string, err error) error {
	if errors.Is(err, clickup.ErrBadRequest) {
		return fmt.Errorf("%w: %s: %w", errChecklistRejected, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}
