package clickup

import (
	"context"
	"fmt"
)

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Space struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type List struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ListStatus struct {
	Status     string `json:"status"`
	Type       string `json:"type"`
	OrderIndex int    `json:"orderindex"`
}

type ListDetail struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Statuses []ListStatus `json:"statuses"`
}

type teamsResponse struct {
	Teams []Workspace `json:"teams"`
}

type spacesResponse struct {
	Spaces []Space `json:"spaces"`
}

type foldersResponse struct {
	Folders []Folder `json:"folders"`
}

type listsResponse struct {
	Lists []List `json:"lists"`
}

func (c *Client) GetWorkspaces(ctx context.Context) ([]Workspace, error) {
	var resp teamsResponse
	err := c.do(ctx, "GET", "/team", nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Teams, nil
}

func (c *Client) GetSpaces(ctx context.Context, workspaceID string) ([]Space, error) {
	var resp spacesResponse
	path := fmt.Sprintf("/team/%s/space", workspaceID)
	err := c.do(ctx, "GET", path, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Spaces, nil
}

func (c *Client) GetFolders(ctx context.Context, spaceID string) ([]Folder, error) {
	var resp foldersResponse
	path := fmt.Sprintf("/space/%s/folder", spaceID)
	err := c.do(ctx, "GET", path, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Folders, nil
}

func (c *Client) GetLists(ctx context.Context, folderID string) ([]List, error) {
	var resp listsResponse
	path := fmt.Sprintf("/folder/%s/list", folderID)
	err := c.do(ctx, "GET", path, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Lists, nil
}

func (c *Client) GetFolderlessLists(ctx context.Context, spaceID string) ([]List, error) {
	var resp listsResponse
	path := fmt.Sprintf("/space/%s/list", spaceID)
	err := c.do(ctx, "GET", path, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Lists, nil
}

func (c *Client) GetListDetails(ctx context.Context, listID string) (*ListDetail, error) {
	var list ListDetail
	path := fmt.Sprintf("/list/%s", listID)
	err := c.do(ctx, "GET", path, nil, &list)
	if err != nil {
		return nil, err
	}
	return &list, nil
}
