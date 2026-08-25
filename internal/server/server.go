package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/b1codes/taskflow/internal/adapters"
	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Server struct {
	mcpServer *server.MCPServer
	service   *session.Service
	store     store.Store
	config    *config.Config
}

func New(st store.Store, cfg *config.Config) *Server {
	s := server.NewMCPServer(
		"taskflow",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	var cuClient session.ClickUpClient
	if cfg.ClickUp.APIKey != "" {
		cuClient = adapters.NewClickUpAdapter(clickup.New(cfg.ClickUp.APIKey))
	}

	srv := &Server{
		mcpServer: s,
		service:   session.NewService(st, adapters.NewGitCapturer(), cuClient),
		store:     st,
		config:    cfg,
	}

	srv.registerTools()

	return srv
}

func (s *Server) Run(ctx context.Context) error {
	stdioServer := server.NewStdioServer(s.mcpServer)
	return stdioServer.Listen(ctx, os.Stdin, os.Stdout)
}

func (s *Server) registerTools() {
	s.mcpServer.AddTool(mcp.NewTool("tf_start",
		mcp.WithDescription("Start or resume a coding session for a ClickUp task"),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("ClickUp task ID (e.g. 86bb06bhb)")),
		mcp.WithString("project_path", mcp.Required(), mcp.Description("Absolute filesystem path to the project directory")),
	), s.handleStart)

	s.mcpServer.AddTool(mcp.NewTool("tf_checkpoint",
		mcp.WithDescription("Record a progress checkpoint for the active session"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithString("summary", mcp.Required(), mcp.Description("Markdown summary of the progress made")),
		mcp.WithArray("files", mcp.WithStringItems(), mcp.Description("Optional list of file paths touched")),
	), s.handleCheckpoint)

	s.mcpServer.AddTool(mcp.NewTool("tf_snag",
		mcp.WithDescription("Log an engineering snag/blocker"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithString("error", mcp.Required(), mcp.Description("The error message or failure description")),
		mcp.WithString("category", mcp.Description("Optional category: build, runtime, test, dependency")),
		mcp.WithString("resolution", mcp.Description("Optional resolution description if already resolved")),
	), s.handleSnag)

	s.mcpServer.AddTool(mcp.NewTool("tf_decision",
		mcp.WithDescription("Log a design choice, trade-off, or non-action justification agreed upon during dialogue"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithString("topic", mcp.Required(), mcp.Description("Topic or subject of the decision (e.g. 'Folder Naming for /internal/gitctx')")),
		mcp.WithString("decision", mcp.Required(), mcp.Description("The agreed decision outcome (e.g. 'Retain current folder name')")),
		mcp.WithString("rationale", mcp.Required(), mcp.Description("Detailed explanation and context for why this choice was made")),
		mcp.WithString("status", mcp.Description("Outcome status: no_change (default), accepted, rejected_alternative, postponed")),
		mcp.WithArray("files", mcp.WithStringItems(), mcp.Description("Optional list of related file or folder paths")),
	), s.handleDecision)

	s.mcpServer.AddTool(mcp.NewTool("tf_list",
		mcp.WithDescription("List coding sessions"),
		mcp.WithString("status", mcp.Description("Optional comma-separated statuses to filter (active, paused, completed, archived)")),
		mcp.WithString("project_path", mcp.Description("Optional project directory path filter")),
	), s.handleList)

	s.mcpServer.AddTool(mcp.NewTool("tf_stop",
		mcp.WithDescription("Stop or pause the session"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The session UUID to stop")),
		mcp.WithString("summary", mcp.Description("Optional final markdown summary of achievements")),
		mcp.WithString("status", mcp.Description("Target status: completed, paused, archived (defaults to completed)")),
	), s.handleStop)

	s.mcpServer.AddTool(mcp.NewTool("tf_init",
		mcp.WithDescription("Initialize ClickUp workspace scan and return cached topology summary"),
		mcp.WithBoolean("refresh", mcp.Description("Force full ClickUp refresh and scan")),
	), s.handleInit)
}

func (s *Server) handleStart(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := request.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	projectPath, err := request.RequireString("project_path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := s.service.Start(ctx, taskID, projectPath, s.config.Git.AutoContext)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(res.AgenticContract, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleCheckpoint(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	summary, err := request.RequireString("summary")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	files := request.GetStringSlice("files", nil)

	cp, err := s.service.Checkpoint(ctx, sessionID, summary, files, s.config.Git.AutoContext)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleSnag(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	errorText, err := request.RequireString("error")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	category := request.GetString("category", "build")
	resolution := request.GetString("resolution", "")

	res, err := s.service.LogSnag(ctx, sessionID, errorText, category, resolution)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleDecision(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	topic, err := request.RequireString("topic")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	decision, err := request.RequireString("decision")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rationale, err := request.RequireString("rationale")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	status := request.GetString("status", "no_change")
	files := request.GetStringSlice("files", nil)

	res, err := s.service.LogDecision(ctx, sessionID, topic, decision, rationale, status, files)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleList(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	statusParam := request.GetString("status", "active,paused")
	projectPath := request.GetString("project_path", "")

	var statuses []session.Status
	if statusParam != "" {
		parts := strings.Split(statusParam, ",")
		for _, part := range parts {
			part = strings.ToUpper(strings.TrimSpace(part))
			if part != "" {
				statuses = append(statuses, session.Status(part))
			}
		}
	}

	filter := session.SessionFilter{
		Status:      statuses,
		ProjectPath: projectPath,
	}

	summaries, err := s.service.List(ctx, filter)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleStop(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	summary := request.GetString("summary", "")
	statusParam := request.GetString("status", "completed")

	targetStatus := strings.ToUpper(statusParam)
	if targetStatus == "" {
		targetStatus = "COMPLETED"
	}

	err = s.service.Stop(ctx, sessionID, summary, targetStatus, s.config.Git.AutoContext)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf(`{"status": "ok", "session_id": %q, "new_status": %q}`, sessionID, targetStatus)), nil
}

func (s *Server) handleInit(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	apiKey := s.config.ClickUp.APIKey
	if apiKey == "" {
		return mcp.NewToolResultError("ClickUp API key is missing. Set CLICKUP_API_KEY environment variable."), nil
	}

	client := clickup.New(apiKey)
	res, err := clickup.ScanWorkspace(ctx, client, s.store)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to scan workspace: %v", err)), nil
	}

	b, err := json.MarshalIndent(struct {
		Status            string `json:"status"`
		WorkspacesScanned int    `json:"workspaces_scanned"`
		SpacesScanned     int    `json:"spaces_scanned"`
		FoldersScanned    int    `json:"folders_scanned"`
		ListsScanned      int    `json:"lists_scanned"`
		StatusesScanned   int    `json:"statuses_scanned"`
	}{
		Status:            "ok",
		WorkspacesScanned: res.WorkspacesCount,
		SpacesScanned:     res.SpacesCount,
		FoldersScanned:    res.FoldersCount,
		ListsScanned:      res.ListsCount,
		StatusesScanned:   res.StatusesCount,
	}, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}
