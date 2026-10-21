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
	return ResolveTargetStatus(ctx, st, client, taskID, role, "", overrides)
}

// ResolveTargetStatus is ResolveStatusForRole with an explicit target status
// the caller asked for (see resolveTarget), which takes the place of the
// role's status_map entry and default; "" resolves the role alone. A target
// the role can't use falls back to the role, with a warning.
func ResolveTargetStatus(ctx context.Context, st store.Store, client *Client, taskID string, role StatusRole, target string, overrides map[StatusRole]string) (string, error) {
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to get task: %w", err)
	}
	name, _, err := resolveStatusForTask(ctx, st, client, task, role, target, overrides, false)
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
	return ApplyTargetStatus(ctx, st, client, taskID, role, "", overrides)
}

// ApplyTargetStatus is ApplyStatusRole with an explicit target status (see
// ResolveTargetStatus); "" applies the role alone.
func ApplyTargetStatus(ctx context.Context, st store.Store, client *Client, taskID string, role StatusRole, target string, overrides map[StatusRole]string) error {
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

	name, fresh, err := resolveStatusForTask(ctx, st, client, task, role, target, overrides, false)
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
	retryName, _, err := resolveStatusForTask(ctx, st, client, task, role, target, overrides, true)
	if err != nil {
		return err
	}
	if strings.EqualFold(retryName, name) {
		// The cached name was current, so the 400 wasn't about it.
		return rejected
	}
	return update(retryName)
}

// resolveStatusForTask is ResolveTargetStatus for an already fetched task, so
// ApplyTargetStatus can re-resolve without fetching the task again.
// forceRefresh bypasses the status cache. fresh reports whether the list's
// statuses were fetched from ClickUp during this call, i.e. whether fetching
// them again could change the result.
func resolveStatusForTask(ctx context.Context, st store.Store, client *Client, task *Task, role StatusRole, target string, overrides map[StatusRole]string, forceRefresh bool) (name string, fresh bool, err error) {
	// A target that names a status only needs the task not done or closed;
	// one the role falls back from, or that names a role, is held to that
	// role's check once it is resolved, below.
	target = strings.TrimSpace(target)
	if target == "" {
		err = checkRoleApplies(role, task.Status)
	} else {
		err = checkTargetApplies(role, target, task.Status)
	}
	if err != nil {
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

	if target != "" {
		// A target names a status someone asked for, so a miss against the
		// cache is worth one refetch: the status may be newer than the cache.
		if !fresh && findTargetStatus(statuses, target) == nil && targetAliasRole(target) == "" {
			if statuses, fresh, err = listStatuses(ctx, st, client, listID, true); err != nil {
				return "", false, err
			}
		}
		var alias StatusRole
		var why string
		name, alias, why = resolveTarget(statuses, role, target)
		switch {
		case alias != "":
			role = alias
		case name == "":
			fmt.Fprintf(os.Stderr, "[clickup] target status %q for task %s %s; using role %q\n", target, task.ID, why, role)
		}
		if name == "" {
			if err := checkRoleApplies(role, task.Status); err != nil {
				return "", fresh, err
			}
		}
	}

	if key, want := overrideForRole(overrides, role); name == "" && want != "" {
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

// findTargetStatus returns the list's status named target (matched
// case-insensitively, "_" also read as a space so "in_review" finds "in
// review"), or nil.
func findTargetStatus(statuses []*store.CacheEntry, target string) *store.CacheEntry {
	for _, want := range []string{target, strings.ReplaceAll(target, "_", " ")} {
		for _, s := range statuses {
			if strings.EqualFold(s.Name, want) {
				return s
			}
		}
	}
	return nil
}

// targetAliasRole returns the role a target names when it is a role's name
// rather than a status's ("review", "completed", "cancelled"), or "".
func targetAliasRole(target string) StatusRole {
	switch strings.Join(statusWords(target), " ") {
	case "review", "in review":
		return RoleReview
	case "complete", "completed", "done":
		return RoleComplete
	case "archive", "archived", "abandon", "abandoned", "cancel", "cancelled", "canceled":
		return RoleArchive
	}
	return ""
}

// resolveTarget resolves an explicit target status for role: the list's
// status of that name, else a role alias (see targetAliasRole), which then
// resolves as its role does, status_map included. Only complete may be
// pointed at a done or closed status, or at another role: the review gate
// holds a task back from being finished, and naming the status it holds the
// task back from mustn't get past it. Returns the status name, or the alias
// role, or neither and why role is used instead.
func resolveTarget(statuses []*store.CacheEntry, role StatusRole, target string) (name string, alias StatusRole, why string) {
	if s := findTargetStatus(statuses, target); s != nil {
		if role != RoleComplete && (s.StatusType == "done" || s.StatusType == "closed") {
			return "", "", fmt.Sprintf("is a %s status, which role %q doesn't move a task to", s.StatusType, role)
		}
		return s.Name, "", ""
	}
	if a := targetAliasRole(target); a != "" {
		if role != RoleComplete && a != role {
			return "", "", fmt.Sprintf("names role %q, which role %q doesn't move a task to", a, role)
		}
		return "", a, ""
	}
	return "", "", "isn't on the list"
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
//	complete → not from done or closed; a task in review or QA is finished
//	           from there (to leave it in review, stop as in_review)
//	review   → not from done, closed, or a review/QA stage: someone already
//	           handed the task off for review/QA or finished it
//	archive  → not from done or closed, so a finished task is never cancelled
func checkRoleApplies(role StatusRole, current TaskStatus) error {
	var applies bool
	switch role {
	case RoleStart:
		applies = current.Type == "open"
	case RoleReopen:
		applies = current.Type == "open" || current.Type == "done" || current.Type == "closed"
	case RoleComplete, RoleArchive:
		applies = current.Type != "done" && current.Type != "closed"
	case RoleReview:
		applies = current.Type != "done" && current.Type != "closed" &&
			!(current.Type == "custom" && isReviewStage(statusWords(current.Status)))
	default:
		return fmt.Errorf("%w: %q", ErrUnknownStatusRole, role)
	}
	if !applies {
		return fmt.Errorf("task is %q (%s), leaving it for role %q: %w", current.Status, current.Type, role, ErrSkipStatusChange)
	}
	return nil
}

// checkTargetApplies is checkRoleApplies for an explicit target status: the
// caller chose where the task goes, so it may move the task from any status
// but done or closed. A target that falls back to a role is checked again as
// that role.
func checkTargetApplies(role StatusRole, target string, current TaskStatus) error {
	if _, err := ParseStatusRole(string(role)); err != nil {
		return err
	}
	if current.Type == "done" || current.Type == "closed" {
		return fmt.Errorf("task is %q (%s), leaving it for target %q: %w", current.Status, current.Type, target, ErrSkipStatusChange)
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
	// completeStatusHints name a done or closed status as the list's completion
	// status, in priority order: "complete"/"completed"/"done" beats a generic
	// status of the same type.
	completeStatusHints = []string{"complete", "completed", "done"}
	// preWorkWords mark statuses for work that hasn't started yet. Lists
	// often add custom ones ("ready for development", "backlog"), which start
	// must never pick. "to do" is matched as a phrase by isPreWork.
	preWorkWords = []string{"ready", "todo", "backlog", "planned", "queued", "next", "new", "open"}
	// notStartPrefixes mark custom statuses that come after work starts.
	notStartPrefixes = []string{"review", "block", "qa", "test", "hold", "wait", "await", "approv"}
	// reviewStagePrefixes mark custom statuses where finished work is being
	// reviewed, tested, or approved before final completion. Review doesn't
	// move a task already in one of these stages back to review.
	reviewStagePrefixes = []string{"review", "qa", "test", "approv"}
	// archivePrefixes mark a closed status as abandoned rather than finished.
	// ClickUp's default lists close finished tasks as "complete", so archiving
	// to just any closed status would report abandoned work as done.
	archivePrefixes = []string{"cancel", "abandon", "wont", "drop", "reject", "obsolete", "duplicate", "archiv"}
)

func isReviewStage(words []string) bool {
	return hasWordWithPrefix(words, reviewStagePrefixes)
}

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
//	complete      → see completeStatus (first done, else closed, preferring
//	                complete/completed/done and excluding cancel/archive names)
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
		return completeStatus(statuses)
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

// completeStatus picks the completion status for RoleComplete:
//  1. A "done" status not named like a cancellation/archive (archivePrefixes),
//     preferring completeStatusHints ("complete", "completed", "done") in
//     priority order before falling back to the first non-archive "done" status.
//  2. Otherwise, a "closed" status not named like a cancellation/archive,
//     again preferring completeStatusHints before falling back to the first
//     non-archive "closed" status.
//
// Otherwise none. A custom status is never picked, even one named
// "complete": reopen only moves a task out of open, done, or closed, so a
// task completed to a custom status would stay there through later work.
func completeStatus(statuses []*store.CacheEntry) string {
	pickFromType := func(statusType string) string {
		type candidate struct {
			name  string
			words []string
		}
		var candidates []candidate
		for _, s := range statuses {
			if s.StatusType != statusType {
				continue
			}
			words := statusWords(s.Name)
			if hasWordWithPrefix(words, archivePrefixes) {
				continue
			}
			candidates = append(candidates, candidate{s.Name, words})
		}
		for _, hint := range completeStatusHints {
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

	if name := pickFromType("done"); name != "" {
		return name
	}
	return pickFromType("closed")
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
