package clickup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

// StatusRole is a session lifecycle event that taskflow maps onto one of a
// list's statuses. ClickUp status types are only open/custom/done/closed and a
// list may have several custom statuses, so a role is resolved from type,
// order, and name rather than by type alone.
type StatusRole string

const (
	RoleStart    StatusRole = "start"
	RoleComplete StatusRole = "complete"
	RoleArchive  StatusRole = "archive"
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
	case RoleStart, RoleComplete, RoleArchive:
		return r, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownStatusRole, s)
}

// LegacyStatusRole maps the pre-role UPDATE_STATUS payload ("status_type"),
// which may still be queued from an older taskflow version, onto a role.
func LegacyStatusRole(statusType string) (StatusRole, error) {
	switch statusType {
	case "active":
		return RoleStart, nil
	case "closed":
		return RoleComplete, nil
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

// statusEntries converts a list's statuses into status cache entries.
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
	return entries
}

// cacheListStatuses replaces the cached statuses of listID, so statuses
// renamed or removed in ClickUp don't linger in the cache.
func cacheListStatuses(ctx context.Context, st store.Store, listID string, statuses []ListStatus) error {
	if err := st.DeleteCacheEntries(ctx, "status", listID); err != nil {
		return fmt.Errorf("failed to clear cached statuses for list %s: %w", listID, err)
	}
	for _, entry := range statusEntries(listID, statuses) {
		if err := st.UpsertCacheEntry(ctx, entry); err != nil {
			return fmt.Errorf("failed to cache status %q for list %s: %w", entry.Name, listID, err)
		}
	}
	return nil
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
	if err := checkRoleApplies(role, task.Status); err != nil {
		return "", err
	}

	listID := task.List.ID
	statuses, err := listStatuses(ctx, st, client, listID)
	if err != nil {
		return "", err
	}

	name := ""
	if want := overrides[role]; want != "" {
		for _, s := range statuses {
			if strings.EqualFold(s.Name, want) {
				name = s.Name
				break
			}
		}
		if name == "" {
			fmt.Fprintf(os.Stderr, "[clickup] status_map.%s = %q not found in list %s; using default\n", role, want, listID)
		}
	}
	if name == "" {
		name = defaultStatusForRole(statuses, role)
	}

	switch {
	case name == "" && role == RoleComplete:
		return "", fmt.Errorf("no matching status found for role %q in list %s: %w", role, listID, ErrNoMatchingStatus)
	case name == "":
		return "", fmt.Errorf("no status for role %q in list %s: %w", role, listID, ErrSkipStatusChange)
	case strings.EqualFold(name, task.Status.Status):
		return "", fmt.Errorf("task %s is already %q: %w", taskID, task.Status.Status, ErrSkipStatusChange)
	}
	return name, nil
}

// checkRoleApplies reports whether a role may move a task out of its current
// status. Returns an ErrSkipStatusChange error when it may not:
//
//	start    → only from open; a task already in progress, in review, or
//	           finished was moved there on purpose and is left alone
//	complete → not from done or closed
//	archive  → not from done or closed, so a finished task is never cancelled
func checkRoleApplies(role StatusRole, current TaskStatus) error {
	var applies bool
	switch role {
	case RoleStart:
		applies = current.Type == "open"
	case RoleComplete, RoleArchive:
		applies = current.Type != "done" && current.Type != "closed"
	default:
		return fmt.Errorf("%w: %q", ErrUnknownStatusRole, role)
	}
	if !applies {
		return fmt.Errorf("task is %q (%s), leaving it for role %q: %w", current.Status, current.Type, role, ErrSkipStatusChange)
	}
	return nil
}

// startStatusHints mark a custom status as the list's working status;
// notStartHints mark custom statuses that come after work starts.
var (
	startStatusHints = []string{"progress", "doing", "develop", "working", "wip", "active"}
	notStartHints    = []string{"review", "block", "qa", "test", "hold", "wait", "approv"}
)

// defaultStatusForRole picks a status from statuses (sorted by orderindex):
//
//	start    → first custom named like "in progress"/"doing", else first
//	           custom not named like a review/QA/blocked stage, else none
//	complete → first done, else first closed
//	archive  → first closed, else none
func defaultStatusForRole(statuses []*store.CacheEntry, role StatusRole) string {
	firstOfType := func(statusType string, match func(name string) bool) string {
		for _, s := range statuses {
			if s.StatusType == statusType && match(strings.ToLower(s.Name)) {
				return s.Name
			}
		}
		return ""
	}
	anyName := func(string) bool { return true }
	containsAny := func(hints []string) func(string) bool {
		return func(name string) bool {
			for _, h := range hints {
				if strings.Contains(name, h) {
					return true
				}
			}
			return false
		}
	}

	switch role {
	case RoleStart:
		if name := firstOfType("custom", containsAny(startStatusHints)); name != "" {
			return name
		}
		isLater := containsAny(notStartHints)
		return firstOfType("custom", func(name string) bool { return !isLater(name) })
	case RoleComplete:
		if name := firstOfType("done", anyName); name != "" {
			return name
		}
		return firstOfType("closed", anyName)
	case RoleArchive:
		return firstOfType("closed", anyName)
	}
	return ""
}

// listStatuses returns listID's statuses sorted by orderindex, served from the
// cache unless missing or older than 24h.
func listStatuses(ctx context.Context, st store.Store, client *Client, listID string) ([]*store.CacheEntry, error) {
	var statuses []*store.CacheEntry
	stale := true
	if st != nil {
		var err error
		statuses, err = st.GetListStatuses(ctx, listID)
		if err != nil {
			return nil, fmt.Errorf("failed to check cached statuses: %w", err)
		}
		if len(statuses) > 0 {
			isStale, err := st.IsCacheStale(ctx, "status", statuses[0].EntityID, 24*time.Hour)
			stale = err != nil || isStale
		}
	}

	if stale {
		details, err := client.GetListDetails(ctx, listID)
		if err != nil {
			return nil, fmt.Errorf("failed to get list details from ClickUp: %w", err)
		}
		statuses = statusEntries(listID, details.Statuses)
		if st != nil {
			if err := cacheListStatuses(ctx, st, listID, details.Statuses); err != nil {
				return nil, err
			}
		}
	}

	sort.SliceStable(statuses, func(i, j int) bool {
		return statuses[i].OrderIndex < statuses[j].OrderIndex
	})
	return statuses, nil
}
