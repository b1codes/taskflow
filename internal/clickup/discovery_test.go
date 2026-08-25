package clickup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_DiscoveryEndpoints(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/team":
			_, _ = w.Write([]byte(`{"teams":[{"id":"ws-1","name":"Workspace 1"}]}`))
		case "/team/ws-1/space":
			_, _ = w.Write([]byte(`{"spaces":[{"id":"sp-1","name":"Space 1"}]}`))
		case "/space/sp-1/folder":
			_, _ = w.Write([]byte(`{"folders":[{"id":"fo-1","name":"Folder 1"}]}`))
		case "/folder/fo-1/list":
			_, _ = w.Write([]byte(`{"lists":[{"id":"li-1","name":"List 1"}]}`))
		case "/space/sp-1/list":
			_, _ = w.Write([]byte(`{"lists":[{"id":"li-folderless","name":"Folderless List"}]}`))
		case "/list/li-1":
			_, _ = w.Write([]byte(`{"id":"li-1","name":"List 1","statuses":[{"status":"Open","type":"open","orderindex":0}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)
	ctx := context.Background()

	// Workspaces
	ws, err := client.GetWorkspaces(ctx)
	if err != nil || len(ws) != 1 || ws[0].ID != "ws-1" {
		t.Fatalf("GetWorkspaces failed: %v, ws=%+v", err, ws)
	}

	// Spaces
	spaces, err := client.GetSpaces(ctx, "ws-1")
	if err != nil || len(spaces) != 1 || spaces[0].ID != "sp-1" {
		t.Fatalf("GetSpaces failed: %v, spaces=%+v", err, spaces)
	}

	// Folders
	folders, err := client.GetFolders(ctx, "sp-1")
	if err != nil || len(folders) != 1 || folders[0].ID != "fo-1" {
		t.Fatalf("GetFolders failed: %v, folders=%+v", err, folders)
	}

	// Lists
	lists, err := client.GetLists(ctx, "fo-1")
	if err != nil || len(lists) != 1 || lists[0].ID != "li-1" {
		t.Fatalf("GetLists failed: %v, lists=%+v", err, lists)
	}

	// Folderless Lists
	flLists, err := client.GetFolderlessLists(ctx, "sp-1")
	if err != nil || len(flLists) != 1 || flLists[0].ID != "li-folderless" {
		t.Fatalf("GetFolderlessLists failed: %v, flLists=%+v", err, flLists)
	}

	// List Details
	details, err := client.GetListDetails(ctx, "li-1")
	if err != nil || details.ID != "li-1" || len(details.Statuses) != 1 {
		t.Fatalf("GetListDetails failed: %v, details=%+v", err, details)
	}
}
