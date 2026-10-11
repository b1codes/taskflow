package clickup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
)

// StatusRole is a session lifecycle event that taskflow maps onto one of a
// list's statuses. ClickUp status types are only open/custom/done/closed and a
// list may have several custom statuses, so a role is resolved from type,
// order, and name rather than by type alone. The roles are defined in session,
// which enqueues them, so both sides share one set of constants.
type StatusRole = session.StatusRole

const (
	RoleStart    = session.RoleStart
	RoleReopen   = session.RoleReopen
	RoleComplete = session.RoleComplete
	RoleReview   = session.RoleReview
	RoleArchive  = session.RoleArchive
)

var (
	ErrUnknownStatusRole = errors.New("unknown status role")
	// ErrSkipStatusChange means the task should be left where it is: its
	// current status already makes the role moot (e.g. starting a task that is
	// in review), or the list has no status suitable for the role.
	ErrSkipStatusChange = errors.New("no status change for role")
)

// ParseStatusRole validates a role name from a sync payload.
func ParseStatusRole(s string) (StatusRole, error) {
	switch r := StatusRole(s); r {
	case RoleStart, RoleReopen, RoleComplete, RoleReview, RoleArchive:
		return r, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownStatusRole, s)
}

// LegacyStatusRole maps the pre-role UPDATE_STATUS payload ("status_type"),
// which may still be queued from an older taskflow version, onto a role. The
// old Stop sent "closed" both when a session was completed and when it was
// archived, so completed tells them apart: it reports whether the task's
// latest session is COMPLETED. Anything else maps to archive, which only ever
// targets a cancel-like status, so abandoned work is never reported as done.
func LegacyStatusRole(statusType string, completed bool) (StatusRole, error) {
	switch statusType {
	case "active":
		return RoleStart, nil
	case "closed":
		if completed {
			return RoleComplete, nil
		}
		return RoleArchive, nil
	}
	return "", fmt.Errorf("%w: legacy status type %q", ErrUnknownStatusRole, statusType)
}

// statusCacheID scopes a status's cache key to its list. Status names like
// "to do" or "complete" repeat across lists, and clickup_cache is unique on
// (entity_type, entity_id), so an unscoped name lets one list's refresh steal
// another list's rows.
func statusCacheID(listID, statusName string) string {
	return listID + "/" + statusName
}

// statusEntries converts a list's statuses into status cache entries sorted
// by orderindex. ClickUp doesn't guarantee the order of its response, and role
// resolution picks the first matching status, so the order is made explicit
// here, once, when the statuses are fetched.
func statusEntries(listID string, statuses []ListStatus) []*store.CacheEntry {
	now := time.Now()
	entries := make([]*store.CacheEntry, 0, len(statuses))
	for _, s := range statuses {
		entries = append(entries, &store.CacheEntry{
			EntityType: "status",
			EntityID:   statusCacheID(listID, s.Status),
			ParentID:   listID,
			Name:       s.Status,
			StatusType: s.Type,
			OrderIndex: s.OrderIndex,
			FetchedAt:  now,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].OrderIndex < entries[j].OrderIndex
	})
	return entries
}

// ResolveStatusForRole returns the name of the status on the task's list that
// the role moves the task to. The task is fetched live so the role is checked
// against its current status: taskflow never moves a task back past a status
// someone else chose (see checkRoleApplies). overrides pins roles to status
// names (matched case-insensitively); an override missing from the list falls
// back to the default with a warning. Returns ErrSkipStatusChange when the
// task should keep its current status.
func ResolveStatusForRole(ctx context.Context, st store.Store, client *Client, taskID string, role StatusRole, overrides map[StatusRole]string) (string, error) {
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to get task: %w", err)
	}
	name, _, err := resolveStatusForTask(ctx, st, client, task, role, overrides, false)
	return name, err
}

// ApplyStatusRole resolves role for the task (see ResolveStatusForRole) and
// moves the task there. Returns an ErrSkipStatusChange error when the task
// keeps its current status.
//
// ClickUp answers 400 when asked for a status the list doesn't have, which
// usually means the status was renamed or removed after it was cached. So when
// the statuses came from the cache, a 400 refreshes them, re-resolves, and
// retries the update once. A 400 against statuses fetched in this call, or one
// the refreshed statuses resolve to the same name, won't be fixed by fetching
// them again. A 400 that ends the update wraps ErrStatusRejected.
func ApplyStatusRole(ctx context.Context, st store.Store, client *Client, taskID string, role StatusRole, overrides map[StatusRole]string) error {
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}
	update := func(name string) error {
		err := client.UpdateTaskStatus(ctx, taskID, name)
		switch {
		case errors.Is(err, ErrBadRequest):
			return fmt.Errorf("failed to move task %s to %q: %w: %w", taskID, name, ErrStatusRejected, err)
		case err != nil:
			return fmt.Errorf("failed to move task %s to %q: %w", taskID, name, err)
		}
		return nil
	}

	name, fresh, err := resolveStatusForTask(ctx, st, client, task, role, overrides, false)
	if err != nil {
		return err
	}
	err = update(name)
	if err == nil || fresh || !errors.Is(err, ErrBadRequest) {
		return err
	}

	// The rejected update left the task where it was, so the task fetched
	// above still holds its current status.
	rejected := err
	retryName, _, err := resolveStatusForTask(ctx, st, client, task, role, overrides, true)
	if err != nil {
		return err
	}
	if strings.EqualFold(retryName, name) {
		// The cached name was current, so the 400 wasn't about it.
		return rejected
	}
	return update(retryName)
}

// resolveStatusForTask is ResolveStatusForRole for an already fetched task, so
// ApplyStatusRole can re-resolve without fetching the task again.
// forceRefresh bypasses the status cache. fresh reports whether the list's
// statuses were fetched from ClickUp during this call, i.e. whether fetching
// them again could change the result.
func resolveStatusForTask(ctx context.Context, st store.Store, client *Client, task *Task, role StatusRole, overrides map[StatusRole]string, forceRefresh bool) (name string, fresh bool, err error) {
	if err := checkRoleApplies(role, task.Status); err != nil {
		return "", false, err
	}

	listID := task.List.ID
	statuses, fresh, err := listStatuses(ctx, st, client, listID, forceRefresh)
	if err != nil {
		return "", false, err
	}
	// The task's live status missing from the cached list proves the cache
	// is stale (a status was added or renamed in ClickUp), however young it
	// is, so don't resolve against it.
	if !fresh && task.Status.Status != "" && findStatus(statuses, task.Status.Status) == "" {
		if statuses, fresh, err = listStatuses(ctx, st, client, listID, true); err != nil {
			return "", false, err
		}
	}

	if key, want := overrideForRole(overrides, role); want != "" {
		name = findStatus(statuses, want)
		// An override names a status someone configured on purpose, so a
		// miss against the cache is worth one refetch before falling back:
		// the status may be newer than the cache. Once a fresh list has
		// confirmed the miss, later ops skip the refetch for a while (see
		// overrideRecheckAfter).
		if name == "" && !fresh && !overrideKnownMissing(listID, key, want) {
			if statuses, fresh, err = listStatuses(ctx, st, client, listID, true); err != nil {
				return "", false, err
			}
			name = findStatus(statuses, want)
		}
		switch {
		case name == "" && fresh:
			noteOverrideMissing(listID, key, want)
		case name != "":
			forgetOverrideMissing(listID, key, want)
		}
	}
	if name == "" {
		name = defaultStatusForRole(statuses, role)
	}

	switch {
	case name == "" && role == RoleComplete:
		return "", fresh, fmt.Errorf("no matching status found for role %q in list %s: %w", role, listID, ErrNoMatchingStatus)
	case name == "":
		return "", fresh, fmt.Errorf("no status for role %q in list %s: %w", role, listID, ErrSkipStatusChange)
	case strings.EqualFold(name, task.Status.Status):
		return "", fresh, fmt.Errorf("task %s is already %q: %w", task.ID, task.Status.Status, ErrSkipStatusChange)
	}
	return name, fresh, nil
}

// findStatus returns the list's spelling of the status named name (matched
// case-insensitively), or "" when the list has no such status.
func findStatus(statuses []*store.CacheEntry, name string) string {
	for _, s := range statuses {
		if strings.EqualFold(s.Name, name) {
			return s.Name
		}
	}
	return ""
}

// overrideForRole returns the status_map key and status name pinned for
// role. Reopen moves a task back to work just as start does, so without an
// override of its own it uses start's.
func overrideForRole(overrides map[StatusRole]string, role StatusRole) (StatusRole, string) {
	if want := overrides[role]; want != "" || role != RoleReopen {
		return role, want
	}
	return RoleStart, overrides[RoleStart]
}

// overrideRecheckAfter is how long a status_map override confirmed missing
// from a freshly fetched list is trusted to stay missing. The sync worker
// resolves a role on every status update, so without it a stale status_map
// entry would refetch the list (and warn) for every task on the list; with
// it, a long-running serve still picks up the status once someone adds it.
const overrideRecheckAfter = 10 * time.Minute

// missingOverrides maps each (list, role, name) override confirmed missing to
// when a fresh list last confirmed it.
var missingOverrides sync.Map

func overrideMissKey(listID string, key StatusRole, want string) string {
	return listID + "\x00" + string(key) + "\x00" + want
}

// overrideKnownMissing reports whether a fresh list confirmed the override
// missing within overrideRecheckAfter.
func overrideKnownMissing(listID string, key StatusRole, want string) bool {
	confirmedAt, ok := missingOverrides.Load(overrideMissKey(listID, key, want))
	return ok && time.Since(confirmedAt.(time.Time)) < overrideRecheckAfter
}

// noteOverrideMissing records that a freshly fetched list lacks the status
// status_map pins key to, and warns the first time it is found missing.
// Reports whether it warned.
func noteOverrideMissing(listID string, key StatusRole, want string) bool {
	if _, seen := missingOverrides.Swap(overrideMissKey(listID, key, want), time.Now()); seen {
		return false
	}
	fmt.Fprintf(os.Stderr, "[clickup] status_map.%s = %q not found in list %s; using default\n", key, want, listID)
	return true
}

// forgetOverrideMissing clears a recorded miss once the override resolves, so
// a later miss warns again.
func forgetOverrideMissing(listID string, key StatusRole, want string) {
	missingOverrides.Delete(overrideMissKey(listID, key, want))
}

// checkRoleApplies reports whether a role may move a task out of its current
// status. Returns an ErrSkipStatusChange error when it may not:
//
//	start    → only from open; a task already in progress, in review, or
//	           finished was moved there on purpose and is left alone
//	reopen   → from open, done, or closed: a new session on a finished task
//	           puts it back to work; a task in progress or in review (custom)
//	           is still being worked and is left alone
//	complete → not from done or closed
//	review   → not from done or closed: someone already finished the task
//	archive  → not from done or closed, so a finished task is never cancelled
func checkRoleApplies(role StatusRole, current TaskStatus) error {
	var applies bool
	switch role {
	case RoleStart:
		applies = current.Type == "open"
	case RoleReopen:
		applies = current.Type == "open" || current.Type == "done" || current.Type == "closed"
	case RoleComplete, RoleReview, RoleArchive:
		applies = current.Type != "done" && current.Type != "closed"
	default:
		return fmt.Errorf("%w: %q", ErrUnknownStatusRole, role)
	}
	if !applies {
		return fmt.Errorf("task is %q (%s), leaving it for role %q: %w", current.Status, current.Type, role, ErrSkipStatusChange)
	}
	return nil
}

// Status names are matched by whole words (or word prefixes), never by
// substring: "inactive" must not read as "active", nor "ready for
// development" as "develop".
var (
	// startStatusHints name a custom status as the list's working status, in
	// priority order: a later "in progress" beats an earlier "doing".
	startStatusHints = []string{"progress", "doing", "wip", "working", "development", "developing", "develop", "active"}
	// preWorkWords mark statuses for work that hasn't started yet. Lists
	// often add custom ones ("ready for development", "backlog"), which start
	// must never pick. "to do" is matched as a phrase by isPreWork.
	preWorkWords = []string{"ready", "todo", "backlog", "planned", "queued", "next", "new", "open"}
	// notStartPrefixes mark custom statuses that come after work starts.
	notStartPrefixes = []string{"review", "block", "qa", "test", "hold", "wait", "await", "approv"}
	// archivePrefixes mark a closed status as abandoned rather than finished.
	// ClickUp's default lists close finished tasks as "complete", so archiving
	// to just any closed status would report abandoned work as done.
	archivePrefixes = []string{"cancel", "abandon", "wont", "drop", "reject", "obsolete", "duplicate", "archiv"}
)

// statusWords lowercases a status name and splits it into words on anything
// but letters and digits. Apostrophes are dropped first so "won't" reads as
// "wont" rather than "won" and "t".
func statusWords(name string) []string {
	name = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(name))
	return strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func hasWordWithPrefix(words, prefixes []string) bool {
	for _, w := range words {
		for _, p := range prefixes {
			if strings.HasPrefix(w, p) {
				return true
			}
		}
	}
	return false
}

func isPreWork(words []string) bool {
	if slices.ContainsFunc(words, func(w string) bool { return slices.Contains(preWorkWords, w) }) {
		return true
	}
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "to" && words[i+1] == "do" {
			return true
		}
	}
	return false
}

// defaultStatusForRole picks a status from statuses (sorted by orderindex):
//
//	start, reopen → see startStatus
//	complete      → first done, else first closed
//	review        → first custom status with the whole word "review" ("in
//	                review", "Code Review"), else none: the task keeps its
//	                status rather than being reported done
//	archive       → first closed named like a cancellation ("cancelled",
//	                "won't do", "duplicate"), else none: a list whose only
//	                closed status means finished has nowhere to archive to
func defaultStatusForRole(statuses []*store.CacheEntry, role StatusRole) string {
	firstOfType := func(statusType string, match func(words []string) bool) string {
		for _, s := range statuses {
			if s.StatusType == statusType && (match == nil || match(statusWords(s.Name))) {
				return s.Name
			}
		}
		return ""
	}

	switch role {
	case RoleStart, RoleReopen:
		return startStatus(statuses)
	case RoleComplete:
		if name := firstOfType("done", nil); name != "" {
			return name
		}
		return firstOfType("closed", nil)
	case RoleReview:
		return firstOfType("custom", func(words []string) bool {
			return slices.Contains(words, "review")
		})
	case RoleArchive:
		return firstOfType("closed", func(words []string) bool {
			return hasWordWithPrefix(words, archivePrefixes)
		})
	}
	return ""
}

// startStatus picks the working status for start and reopen from the custom
// statuses that are neither pre-work ("ready", "backlog") nor a later stage
// (review/QA/blocked): the one matching the highest-priority start hint,
// wherever it sits in the list, else the first of them, else none.
func startStatus(statuses []*store.CacheEntry) string {
	type candidate struct {
		name  string
		words []string
	}
	var candidates []candidate
	for _, s := range statuses {
		if s.StatusType != "custom" {
			continue
		}
		words := statusWords(s.Name)
		if isPreWork(words) || hasWordWithPrefix(words, notStartPrefixes) {
			continue
		}
		candidates = append(candidates, candidate{s.Name, words})
	}

	for _, hint := range startStatusHints {
		for _, c := range candidates {
			if slices.Contains(c.words, hint) {
				return c.name
			}
		}
	}
	if len(candidates) > 0 {
		return candidates[0].name
	}
	return ""
}

// listStatuses returns listID's statuses sorted by orderindex, served from the
// cache unless forceRefresh is set or the cache is missing or older than 24h.
// fresh reports whether they were fetched from ClickUp in this call.
func listStatuses(ctx context.Context, st store.Store, client *Client, listID string, forceRefresh bool) (statuses []*store.CacheEntry, fresh bool, err error) {
	if st != nil && !forceRefresh {
		cached, err := st.GetListStatuses(ctx, listID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to check cached statuses: %w", err)
		}
		if len(cached) > 0 {
			// GetListStatuses already orders by orderindex.
			if stale, err := st.IsCacheStale(ctx, "status", cached[0].EntityID, 24*time.Hour); err == nil && !stale {
				return cached, false, nil
			}
		}
	}

	details, err := client.GetListDetails(ctx, listID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get list details from ClickUp: %w", err)
	}
	statuses = statusEntries(listID, details.Statuses)
	if st != nil {
		// One transaction, so statuses renamed or removed in ClickUp don't
		// linger and a failed refresh never leaves a partial set that looks
		// fresh.
		if err := st.ReplaceListStatuses(ctx, listID, statuses); err != nil {
			return nil, false, err
		}
	}
	return statuses, true, nil
}
