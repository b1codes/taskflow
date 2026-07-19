package clickup

import (
	"context"
	"fmt"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

func ResolveStatus(ctx context.Context, st store.Store, client *Client, taskID string, targetType string) (string, error) {
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to get task: %w", err)
	}

	listID := task.List.ID

	statuses, err := st.GetListStatuses(ctx, listID)
	if err != nil {
		return "", fmt.Errorf("failed to check cached statuses: %w", err)
	}

	stale := false
	if len(statuses) == 0 {
		stale = true
	} else {
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
	}

	for _, s := range statuses {
		if s.StatusType == targetType {
			return s.Name, nil
		}
	}

	return "", nil
}
