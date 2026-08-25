package clickup

import (
	"context"
	"fmt"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

func ResolveStatus(ctx context.Context, st store.Store, client *Client, taskID string, targetType string) (string, error) {
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
			return "", fmt.Errorf("failed to get task: %w", err)
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
			return "", fmt.Errorf("failed to check cached statuses: %w", err)
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
			return "", fmt.Errorf("failed to get list details from ClickUp: %w", err)
		}

		if st != nil {
			_ = st.DeleteCacheEntries(ctx, "status", listID)

			for _, stItem := range details.Statuses {
				err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
					EntityType: "status",
					EntityID:   stItem.Status,
					ParentID:   listID,
					Name:       stItem.Status,
					StatusType: stItem.Type,
					OrderIndex: stItem.OrderIndex,
					FetchedAt:  time.Now(),
				})
				if err != nil {
					return "", fmt.Errorf("failed to cache list status: %w", err)
				}
			}

			statuses, err = st.GetListStatuses(ctx, listID)
			if err != nil {
				return "", fmt.Errorf("failed to get list statuses after refresh: %w", err)
			}
		} else {
			for _, stItem := range details.Statuses {
				statuses = append(statuses, &store.CacheEntry{
					EntityType: "status",
					EntityID:   stItem.Status,
					ParentID:   listID,
					Name:       stItem.Status,
					StatusType: stItem.Type,
					OrderIndex: stItem.OrderIndex,
				})
			}
		}
	}

	for _, s := range statuses {
		if s.StatusType == targetType {
			return s.Name, nil
		}
	}

	return "", fmt.Errorf("no matching status found for type %q in list %s: %w", targetType, listID, ErrNoMatchingStatus)
}
