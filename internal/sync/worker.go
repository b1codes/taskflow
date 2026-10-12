package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
	// pruneEvery is how often Run prunes the sync queue.
	pruneEvery time.Duration
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
		store:      st,
		clickup:    client,
		config:     cfg,
		pruneEvery: pruneInterval,
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

// pruneInterval is how often Run prunes finished ops from the sync queue.
const pruneInterval = 24 * time.Hour

// Prune deletes ops that finished longer ago than the configured retention
// from the sync queue; with no retention it deletes nothing. The checklist
// history earlierMatches reads is kept apart, so it isn't pruned.
func (w *Worker) Prune(ctx context.Context) (int64, error) {
	if w.config == nil || w.config.Retention() <= 0 {
		return 0, nil
	}
	return w.store.PruneSyncQueue(ctx, w.config.Retention())
}

func (w *Worker) prune(ctx context.Context) {
	if _, err := w.Prune(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[sync] prune error: %v\n", err)
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.clickup == nil {
		// With no ClickUp client configured nothing syncs, so wait for
		// shutdown without spinning; still prune, since a queue filled before
		// the key was removed would otherwise never shrink.
		prune := time.NewTicker(w.pruneEvery)
		defer prune.Stop()
		for {
			w.prune(ctx)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-prune.C:
			}
		}
	}

	// Reset any stale in-flight ops on worker startup
	_, _ = w.store.ResetStaleInFlight(ctx, 5*time.Minute)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastPrune time.Time

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
				// Pruning waits for an idle pass, so it never holds back
				// ops waiting to sync (the first one, at startup, included).
				if time.Since(lastPrune) >= w.pruneEvery {
					w.prune(ctx)
					lastPrune = time.Now()
				}
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

	claimed, err := w.store.MarkInFlight(ctx, op.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync] failed to mark op %d in-flight: %v\n", op.ID, err)
		return err
	}
	if claimed == nil {
		// Another worker claimed it, or it finished, since it was listed.
		return errNotClaimed
	}
	// The claimed row, not the listed one: a full-state op's payload may
	// have been replaced in between (CoalesceSyncOp).
	op = claimed

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

	if errors.Is(opErr, errSuperseded) {
		// Recorded apart from DONE: a skipped op applied nothing, so
		// HasNewerOp must not count it as holding back older ops. It still
		// returns an error, so it isn't counted as synced.
		if err := w.store.MarkSkipped(ctx, op.ID, opErr.Error()); err != nil {
			return err
		}
		return opErr
	}

	var short *checklistShortfall
	if errors.As(opErr, &short) {
		fmt.Fprintf(os.Stderr, "[sync] op %d (task %s) applied in part: %v\n", op.ID, op.TaskID, short)
		if len(short.unresolved) > 0 {
			return w.store.MarkPartial(ctx, op.ID, short.Error())
		}
		return w.store.MarkDone(ctx, op.ID, short.Error())
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

	return w.store.MarkDone(ctx, op.ID, "")
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
	// errSuperseded means the op was skipped for a newer op of the same
	// operation for the task; processOp marks it SKIPPED, not failed.
	errSuperseded = errors.New("superseded by a newer op")
	// errNotClaimed means the op was no longer waiting to run when the
	// worker went to claim it.
	errNotClaimed = errors.New("op no longer waiting to run")
)

// checklistShortfall is a SYNC_CHECKLIST op that applied all but some items:
// rejected ones, whose create or update ClickUp refused with a 400 (the same
// write would be refused again, so the op is DONE and only a changed
// payload retries them), and unresolved ones, created but not identifiable
// in ClickUp's response to resolve (the op is PARTIAL, so the next
// evaluation queues the checklist again and finds them by name).
type checklistShortfall struct {
	rejected   []string
	unresolved []string
}

func (e *checklistShortfall) Error() string {
	var parts []string
	if len(e.rejected) > 0 {
		parts = append(parts, "rejected by ClickUp: "+strings.Join(e.rejected, "; "))
	}
	if len(e.unresolved) > 0 {
		parts = append(parts, "created but left unresolved: "+strings.Join(e.unresolved, "; "))
	}
	return strings.Join(parts, "; ")
}

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
// are only renamed, never created. The op is skipped (errSuperseded, marked
// SKIPPED) when a newer op for the task has already been applied or still
// will be, so a retried stale op never undoes a later state. An item ClickUp
// rejects doesn't stop the others: they are all applied, and the op then
// reports a *checklistShortfall.
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
		return errSuperseded
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

	matched := matchChecklistItems(cl.Items, p.Items, func() map[string][]string { return w.earlierMatches(ctx, op) })
	// known holds every item ID seen so far, so the item a create adds is the
	// one ID new in its response, whatever ClickUp did to its name.
	known := make(map[string]bool, len(cl.Items))
	for _, ci := range cl.Items {
		known[ci.ID] = true
	}
	var short checklistShortfall
	for i, it := range p.Items {
		ex, ok := matched[i]
		if !ok {
			if it.Void {
				continue
			}
			updated, err := w.clickup.CreateChecklistItem(ctx, cl.ID, it.Name)
			if errors.Is(err, clickup.ErrBadRequest) {
				short.rejected = append(short.rejected, fmt.Sprintf("creating %q: %v", it.Name, err))
				continue
			}
			if err != nil {
				return checklistWriteError(fmt.Sprintf("creating checklist item %q", it.Name), err)
			}
			added := newItems(updated.Items, known)
			if !it.Resolved {
				continue
			}
			// New items start unresolved; find this one to resolve it.
			ex, ok = createdItem(added, it)
			if !ok && len(added) == 0 {
				// The response left the new item out: read it back.
				if added, err = w.readBackItems(ctx, op.TaskID, cl.ID, known); err == nil {
					ex, ok = createdItem(added, it)
				}
			}
			if !ok {
				// Once created, an item must not be created again, so this
				// isn't an error retried at once: a retry that can't match
				// the item by name either would create a duplicate on every
				// attempt. The op is PARTIAL instead, so the next
				// evaluation queues the checklist again to resolve it.
				why := fmt.Sprintf("%d new items, none identifiable", len(added))
				if err != nil {
					why = err.Error()
				}
				short.unresolved = append(short.unresolved, fmt.Sprintf("%q (%s)", it.Name, why))
				continue
			}
		}
		if !sameChecklistName(ex.Name, it.Name) || ex.Resolved != it.Resolved {
			err := w.clickup.UpdateChecklistItem(ctx, cl.ID, ex.ID, it.Name, it.Resolved)
			if errors.Is(err, clickup.ErrBadRequest) {
				short.rejected = append(short.rejected, fmt.Sprintf("updating %q: %v", it.Name, err))
				continue
			}
			if err != nil {
				return checklistWriteError(fmt.Sprintf("updating checklist item %q", it.Name), err)
			}
		}
	}
	if len(short.rejected) > 0 || len(short.unresolved) > 0 {
		return &short
	}
	return nil
}

// newItems returns the items whose IDs aren't in known, adding them to it.
func newItems(items []clickup.ChecklistItem, known map[string]bool) []clickup.ChecklistItem {
	var added []clickup.ChecklistItem
	for _, ci := range items {
		if !known[ci.ID] {
			known[ci.ID] = true
			added = append(added, ci)
		}
	}
	return added
}

// readBackItems re-reads checklist id and returns its items new since known
// (see newItems), for a create whose response left the new item out.
func (w *Worker) readBackItems(ctx context.Context, taskID, id string, known map[string]bool) ([]clickup.ChecklistItem, error) {
	checklists, err := w.clickup.GetTaskChecklists(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for _, cl := range checklists {
		if cl.ID == id {
			return newItems(cl.Items, known), nil
		}
	}
	return nil, nil
}

// createdItem picks the item a create of it added from the items new since
// it: the only one, else (someone else added items meanwhile) the one named
// it.Name, else the only one with its stable part. With none, or several it
// can't tell apart, it reports false rather than guess at another's item.
func createdItem(added []clickup.ChecklistItem, it session.ChecklistItem) (clickup.ChecklistItem, bool) {
	if len(added) == 1 {
		return added[0], true
	}
	for _, ci := range added {
		if sameChecklistName(ci.Name, it.Name) {
			return ci, true
		}
	}
	var found []clickup.ChecklistItem
	for _, ci := range added {
		if session.ChecklistItemMatches(ci.Name, it.Match) {
			found = append(found, ci)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return clickup.ChecklistItem{}, false
}

// sameChecklistName compares names as ClickUp may store them (whitespace
// collapsed and trimmed), so a name it normalized isn't renamed on every sync.
func sameChecklistName(a, b string) bool {
	return session.NormalizeChecklistName(a) == session.NormalizeChecklistName(b)
}

// maxEarlierChecklistOps caps how many of a task's earlier SYNC_CHECKLIST ops
// are read for the Match values its items had before: all the history keeps.
const maxEarlierChecklistOps = store.MaxSyncHistory

// earlierMatches maps each obligation Key to the Match values its item had in
// the task's earlier checklist ops, most recent first, so an item whose
// describe text changed in the manifest is still found without storing
// ClickUp IDs. Only ops that applied (DONE, PARTIAL, the sync history) count:
// a skipped, failed, or still-waiting op may have written nothing, so it is
// no evidence of whose an item is. A failed lookup only loses that matching
// pass, so it is logged rather than failing the sync.
func (w *Worker) earlierMatches(ctx context.Context, op *session.SyncOp) map[string][]string {
	ops, err := w.store.ListSyncHistory(ctx, op.TaskID, op.Operation, op.ID, maxEarlierChecklistOps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync] couldn't list earlier checklist ops for task %s: %v\n", op.TaskID, err)
		return nil
	}
	out := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, o := range ops { // newest first
		var p session.ChecklistPayload
		if json.Unmarshal([]byte(o.Payload), &p) != nil {
			continue
		}
		for _, it := range p.Items {
			k := [2]string{it.Key, session.NormalizeChecklistName(it.Match)}
			if it.Key == "" || k[1] == "" || seen[k] {
				continue
			}
			seen[k] = true
			out[it.Key] = append(out[it.Key], k[1])
		}
	}
	return out
}

// matchChecklistItems pairs each wanted item with an existing ClickUp item,
// each existing item used at most once. When every wanted item has an item of
// exactly its name, those are used and nothing changes; likewise when only
// void items are left unmatched and no existing item is left over for them
// (a void item that was never created has nothing to find, and void items
// are never created), so such an item doesn't send every later sync through
// the history read. Otherwise an item
// goes first to the obligation Key whose earlier ops (earlier, read only then)
// had its stable part (session.ChecklistItemMatches): each Key tries the
// Match values it had, most recent first, one rank at a time across Keys, so
// a Key whose describe changed X→Y takes its Y item before an X item now
// another Key's, and an item whose describe text changed is renamed. Then by
// name, then by the wanted item's current stable part, so an item whose
// status text changed is updated rather than duplicated. History goes before
// the current name and Match because they are no evidence of whose an item
// is: a new Key rendering an edited Key's old text would otherwise take that
// Key's item, and the edited Key would create a duplicate. Within a stable
// part, an item of exactly the wanted name is preferred.
func matchChecklistItems(existing []clickup.ChecklistItem, want []session.ChecklistItem, earlier func() map[string][]string) map[int]clickup.ChecklistItem {
	matched := map[int]clickup.ChecklistItem{}
	used := map[string]bool{}
	// claim gives want[i], unless it has one, the first unused item ok takes.
	claim := func(i int, ok func(name string) bool) {
		if _, done := matched[i]; done {
			return
		}
		for _, ci := range existing {
			if !used[ci.ID] && ok(ci.Name) {
				matched[i], used[ci.ID] = ci, true
				return
			}
		}
	}
	byName := func() {
		for i := range want {
			claim(i, func(name string) bool { return sameChecklistName(name, want[i].Name) })
		}
	}
	byMatch := func(i int, m string) {
		claim(i, func(name string) bool {
			return sameChecklistName(name, want[i].Name) && session.ChecklistItemMatches(name, m)
		})
		claim(i, func(name string) bool { return session.ChecklistItemMatches(name, m) })
	}

	byName()
	if len(matched) == len(want) || len(existing) == 0 || (len(used) == len(existing) && onlyVoidUnmatched(want, matched)) {
		return matched
	}
	var history map[string][]string
	if earlier != nil {
		history = earlier()
	}
	if len(history) > 0 {
		// Start over so history decides before names do.
		matched, used = map[int]clickup.ChecklistItem{}, map[string]bool{}
		for rank, more := 0, true; more; rank++ {
			more = false
			for i, it := range want {
				if ms := history[it.Key]; rank < len(ms) {
					more = true
					byMatch(i, ms[rank])
				}
			}
		}
		byName()
	}
	for i, it := range want {
		byMatch(i, it.Match)
	}
	return matched
}

// onlyVoidUnmatched reports whether every wanted item without a match is void.
func onlyVoidUnmatched(want []session.ChecklistItem, matched map[int]clickup.ChecklistItem) bool {
	for i, it := range want {
		if _, ok := matched[i]; !ok && !it.Void {
			return false
		}
	}
	return true
}

// checklistWriteError wraps a failed checklist write; a 400 is permanent,
// since the same write would be rejected again. (A 400 to an item's write
// only skips that item: see checklistShortfall.)
func checklistWriteError(what string, err error) error {
	if errors.Is(err, clickup.ErrBadRequest) {
		return fmt.Errorf("%w: %s: %w", errChecklistRejected, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}
