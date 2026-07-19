package clickup

import (
	"context"
	"fmt"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

type ScanResult struct {
	WorkspacesCount int
	SpacesCount     int
	FoldersCount    int
	ListsCount      int
	StatusesCount   int
}

func ScanWorkspace(ctx context.Context, client *Client, st store.Store) (*ScanResult, error) {
	result := &ScanResult{}

	delay := func() {
		select {
		case <-ctx.Done():
		case <-time.After(600 * time.Millisecond):
		}
	}

	workspaces, err := client.GetWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to scan workspaces: %w", err)
	}
	result.WorkspacesCount = len(workspaces)
	delay()

	for _, ws := range workspaces {
		err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
			EntityType: "workspace",
			EntityID:   ws.ID,
			Name:       ws.Name,
			FetchedAt:  time.Now(),
		})
		if err != nil {
			return nil, err
		}

		spaces, err := client.GetSpaces(ctx, ws.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to scan spaces for workspace %s: %w", ws.ID, err)
		}
		result.SpacesCount += len(spaces)
		delay()

		for _, sp := range spaces {
			err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
				EntityType: "space",
				EntityID:   sp.ID,
				ParentID:   ws.ID,
				Name:       sp.Name,
				FetchedAt:  time.Now(),
			})
			if err != nil {
				return nil, err
			}

			folderlessLists, err := client.GetFolderlessLists(ctx, sp.ID)
			if err != nil {
				return nil, fmt.Errorf("failed to scan folderless lists for space %s: %w", sp.ID, err)
			}
			result.ListsCount += len(folderlessLists)
			delay()

			for _, l := range folderlessLists {
				err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
					EntityType: "list",
					EntityID:   l.ID,
					ParentID:   sp.ID,
					Name:       l.Name,
					FetchedAt:  time.Now(),
				})
				if err != nil {
					return nil, err
				}

				details, err := client.GetListDetails(ctx, l.ID)
				if err != nil {
					return nil, fmt.Errorf("failed to scan list details for list %s: %w", l.ID, err)
				}
				result.StatusesCount += len(details.Statuses)
				delay()

				for _, stItem := range details.Statuses {
					err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
						EntityType: "status",
						EntityID:   stItem.Status,
						ParentID:   l.ID,
						Name:       stItem.Status,
						StatusType: stItem.Type,
						OrderIndex: stItem.OrderIndex,
						FetchedAt:  time.Now(),
					})
					if err != nil {
						return nil, err
					}
				}
			}

			folders, err := client.GetFolders(ctx, sp.ID)
			if err != nil {
				return nil, fmt.Errorf("failed to scan folders for space %s: %w", sp.ID, err)
			}
			result.FoldersCount += len(folders)
			delay()

			for _, f := range folders {
				err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
					EntityType: "folder",
					EntityID:   f.ID,
					ParentID:   sp.ID,
					Name:       f.Name,
					FetchedAt:  time.Now(),
				})
				if err != nil {
					return nil, err
				}

				lists, err := client.GetLists(ctx, f.ID)
				if err != nil {
					return nil, fmt.Errorf("failed to scan lists for folder %s: %w", f.ID, err)
				}
				result.ListsCount += len(lists)
				delay()

				for _, l := range lists {
					err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
						EntityType: "list",
						EntityID:   l.ID,
						ParentID:   f.ID,
						Name:       l.Name,
						FetchedAt:  time.Now(),
					})
					if err != nil {
						return nil, err
					}

					details, err := client.GetListDetails(ctx, l.ID)
					if err != nil {
						return nil, fmt.Errorf("failed to scan list details for list %s: %w", l.ID, err)
					}
					result.StatusesCount += len(details.Statuses)
					delay()

					for _, stItem := range details.Statuses {
						err = st.UpsertCacheEntry(ctx, &store.CacheEntry{
							EntityType: "status",
							EntityID:   stItem.Status,
							ParentID:   l.ID,
							Name:       stItem.Status,
							StatusType: stItem.Type,
							OrderIndex: stItem.OrderIndex,
							FetchedAt:  time.Now(),
						})
						if err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}

	return result, nil
}
