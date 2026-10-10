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
	RoleReview   StatusRole = "review"
	RoleComplete StatusRole = "complete"
	RoleArchive  StatusRole = "archive"
)

var (
	ErrUnknownStatusRole = errors.New("unknown status role")
	// ErrSkipStatusChange means the role has no suitable status on the list and
	// the task should be left where it is (e.g. no "review" status exists).
	ErrSkipStatusChange = errors.New("no status change for role")
)

// ParseStatusRole validates a role name from a sync payload.
func ParseStatusRole(s string) (StatusRole, error) {
	switch r := StatusRole(s); r {
	case RoleStart, RoleReview, RoleComplete, RoleArchive:
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

// ResolveStatusForRole returns the name of the status on the task's list that
// the role maps to. overrides pins roles to status names (matched
// case-insensitively); an override missing from the list falls back to the
// default with a warning. Returns ErrSkipStatusChange when the role should
// leave the task's status unchanged.
func ResolveStatusForRole(ctx context.Context, st store.Store, client *Client, taskID string, role StatusRole, overrides map[StatusRole]string) (string, error) {
	if _, err := ParseStatusRole(string(role)); err != nil {
		return "", err
	}

	listID, statuses, err := listStatuses(ctx, st, client, taskID)
	if err != nil {
		return "", err
	}

	if want := overrides[role]; want != "" {
		for _, s := range statuses {
			if strings.EqualFold(s.Name, want) {
				return s.Name, nil
			}
		}
		fmt.Fprintf(os.Stderr, "[clickup] status_map.%s = %q not found in list %s; using default\n", role, want, listID)
	}

	if name := defaultStatusForRole(statuses, role); name != "" {
		return name, nil
	}
	if role == RoleReview {
		return "", fmt.Errorf("no review status in list %s: %w", listID, ErrSkipStatusChange)
	}
	return "", fmt.Errorf("no matching status found for role %q in list %s: %w", role, listID, ErrNoMatchingStatus)
}

// defaultStatusForRole picks a status from statuses (sorted by orderindex):
//
//	start    → first custom, else first open
//	review   → first custom whose name contains "review"
//	complete → first done, else first closed
//	archive  → first closed
func defaultStatusForRole(statuses []*store.CacheEntry, role StatusRole) string {
	firstOfType := func(statusType string) string {
		for _, s := range statuses {
			if s.StatusType == statusType {
				return s.Name
			}
		}
		return ""
	}

	switch role {
	case RoleStart:
		if name := firstOfType("custom"); name != "" {
			return name
		}
		return firstOfType("open")
	case RoleReview:
		for _, s := range statuses {
			if s.StatusType == "custom" && strings.Contains(strings.ToLower(s.Name), "review") {
				return s.Name
			}
		}
		return ""
	case RoleComplete:
		if name := firstOfType("done"); name != "" {
			return name
		}
		return firstOfType("closed")
	case RoleArchive:
		return firstOfType("closed")
	}
	return ""
}

// listStatuses returns the task's list ID and that list's statuses sorted by
// orderindex, served from the cache unless missing or older than 24h.
func listStatuses(ctx context.Context, st store.Store, client *Client, taskID string) (string, []*store.CacheEntry, error) {
	var listID string

	// 1. Check if task -> list ID mapping is cached
	if st != nil {
		entry, err := st.GetCacheEntry(ctx, "task", taskID)
		if err == nil && entry != nil && entry.ParentID != "" {
			stale, err := st.IsCacheStale(ctx, "task", taskID, 24*time.Hour)
			if err == nil && !stale {
				listID = entry.ParentID
			}
		}
	}

	// 2. Fetch from ClickUp if not cached or stale
	if listID == "" {
		task, err := client.GetTask(ctx, taskID)
		if err != nil {
			return "", nil, fmt.Errorf("failed to get task: %w", err)
		}
		listID = task.List.ID

		if st != nil {
			_ = st.UpsertCacheEntry(ctx, &store.CacheEntry{
				EntityType: "task",
				EntityID:   taskID,
				ParentID:   listID,
				Name:       task.Name,
				FetchedAt:  time.Now(),
			})
		}
	}

	var statuses []*store.CacheEntry
	if st != nil {
		var err error
		statuses, err = st.GetListStatuses(ctx, listID)
		if err != nil {
			return "", nil, fmt.Errorf("failed to check cached statuses: %w", err)
		}
	}

	stale := false
	if len(statuses) == 0 {
		stale = true
	} else if st != nil {
		isStale, err := st.IsCacheStale(ctx, "status", statuses[0].EntityID, 24*time.Hour)
		if err != nil || isStale {
			stale = true
		}
	}

	if stale {
		details, err := client.GetListDetails(ctx, listID)
		if err != nil {
			return "", nil, fmt.Errorf("failed to get list details from ClickUp: %w", err)
		}

		if st != nil {
			_ = st.DeleteCacheEntries(ctx, "status", listID)

			for _, stItem := range details.Statuses {
				err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
					EntityType: "status",
					EntityID:   statusCacheID(listID, stItem.Status),
					ParentID:   listID,
					Name:       stItem.Status,
					StatusType: stItem.Type,
					OrderIndex: stItem.OrderIndex,
					FetchedAt:  time.Now(),
				})
				if err != nil {
					return "", nil, fmt.Errorf("failed to cache list status: %w", err)
				}
			}

			statuses, err = st.GetListStatuses(ctx, listID)
			if err != nil {
				return "", nil, fmt.Errorf("failed to get list statuses after refresh: %w", err)
			}
		} else {
			for _, stItem := range details.Statuses {
				statuses = append(statuses, &store.CacheEntry{
					EntityType: "status",
					EntityID:   statusCacheID(listID, stItem.Status),
					ParentID:   listID,
					Name:       stItem.Status,
					StatusType: stItem.Type,
					OrderIndex: stItem.OrderIndex,
				})
			}
		}
	}

	sort.SliceStable(statuses, func(i, j int) bool {
		return statuses[i].OrderIndex < statuses[j].OrderIndex
	})
	return listID, statuses, nil
}
