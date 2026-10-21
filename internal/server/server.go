package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// New builds the MCP server. client may be nil (no ClickUp sync); serve passes
// the same paced client it gives the sync worker, so both share one
// rate-limit budget. It fails on an invalid completion gate in cfg.
func New(st store.Store, cfg *config.Config, client *clickup.Client) (*Server, error) {
	gate, err := cfg.Completeness.GateMode()
	if err != nil {
		return nil, err
	}

	s := server.NewMCPServer(
		"taskflow",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	var cuClient session.ClickUpClient
	if client != nil {
		cuClient = adapters.NewClickUpAdapter(client)
	}

	srv := &Server{
		mcpServer: s,
		service: session.NewService(st, adapters.NewGitCapturer(), cuClient).
			WithCompleteness(adapters.NewCompletenessEvaluator().
				WithCommands(cfg.Completeness.AllowCommands, cfg.Completeness.CommandTimeout())).
			WithGate(gate),
		store:  st,
		config: cfg,
	}

	srv.registerTools()

	return srv, nil
}

func (s *Server) Run(ctx context.Context) error {
	return s.RunReader(ctx, os.Stdin, os.Stdout)
}

func (s *Server) RunReader(ctx context.Context, in io.Reader, out io.Writer) error {
	stdioServer := server.NewStdioServer(s.mcpServer)
	return stdioServer.Listen(ctx, in, out)
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
		mcp.WithDescription("Stop or pause the session. Completing it runs the completeness command checks (when enabled), and with required obligations open goes through the project's completion gate: it may move the ClickUp task to review, or (gate: block) refuse until each one is done, attested, or waived"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The session UUID to stop")),
		mcp.WithString("summary", mcp.Description("Optional final markdown summary of achievements")),
		mcp.WithString("status", mcp.Description("Target status: completed (or in_review/review to complete the session but move the ClickUp task to review, e.g. when it awaits a PR review; completed moves it to complete even from review), paused, archived (or abandoned/cancelled to move the ClickUp task to a cancel-like status). Defaults to completed")),
		mcp.WithString("target_task_status", mcp.Description("Optional ClickUp status to move the task to when completing (e.g. 'in review', 'testing/qa'); only with status completed or in_review. Falls back to the default when the list has no such status, and the review gate never lets it move the task to a done/closed status")),
	), s.handleStop)

	s.mcpServer.AddTool(mcp.NewTool("tf_completeness",
		mcp.WithDescription("Re-check the session's changes against the project's completeness manifest and report what is still owed (tests, stories, contracts)"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The session UUID")),
	), s.handleCompleteness)

	s.mcpServer.AddTool(mcp.NewTool("tf_verify",
		mcp.WithDescription("Run the session's completeness command checks now (e.g. the stack's test suite or Storybook test-runner, from the project's manifest) rather than waiting for tf_stop, and report the results. A failing command keeps its obligation open, with its output. Needs command checks enabled in the user's taskflow config"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
	), s.handleVerify)

	s.mcpServer.AddTool(mcp.NewTool("tf_waive",
		mcp.WithDescription("Waive a completeness obligation that genuinely does not apply. The rationale is logged as a decision and posted to the ClickUp task"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithString("obligation_id", mcp.Required(), mcp.Description("The obligation_id from tf_checkpoint, tf_completeness, or the start contract")),
		mcp.WithString("rationale", mcp.Required(), mcp.Description("Why the requirement doesn't apply to this change")),
	), s.handleWaive)

	s.mcpServer.AddTool(mcp.NewTool("tf_attest",
		mcp.WithDescription("Attest that a completeness obligation is met, with evidence taskflow can't check from files (e.g. which spec and test cases cover a screen, or a test run's summary line). Only obligations whose requirement asks for an attestation (they carry an `attest` prompt) can be attested; one checked from files is refused: do the work, or tf_waive it with a rationale. Attestations are shown in ClickUp for human review"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithString("obligation_id", mcp.Required(), mcp.Description("The obligation_id from tf_checkpoint, tf_completeness, or the start contract")),
		mcp.WithString("evidence", mcp.Required(), mcp.Description("What shows the obligation is met, answering its attest prompt")),
	), s.handleAttest)

	s.mcpServer.AddTool(mcp.NewTool("tf_plan",
		mcp.WithDescription("Declare work you expect to do before its files exist (plannable rules only), so its completeness obligations are tracked from the start; or decline the suggested plan with decline_reason when none applies"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The active session UUID")),
		mcp.WithArray("items", mcp.Description("Planned subjects: each names a plannable rule and the vars its file pattern takes, e.g. {\"rule_id\": \"react-screen\", \"vars\": {\"feature\": \"payments\", \"name\": \"CheckoutPage\"}}"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"rule_id": map[string]any{"type": "string"},
					"vars":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				},
				"required": []string{"rule_id"},
			})),
		mcp.WithString("decline_reason", mcp.Description("Instead of items: why no up-front plan applies to this task (logged as a decision)")),
	), s.handlePlan)

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
	targetTaskStatus := request.GetString("target_task_status", "")

	targetStatus := strings.ToUpper(statusParam)
	if targetStatus == "" {
		targetStatus = "COMPLETED"
	}

	// MCP has no override for the block gate: an agent gets past it only by
	// doing, attesting, or waiving the work.
	res, err := s.service.StopWithTaskStatus(ctx, sessionID, summary, targetStatus, targetTaskStatus, s.config.Git.AutoContext, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return jsonResult(struct {
		Status string `json:"status"`
		*session.StopResult
	}{Status: "ok", StopResult: res})
}

func (s *Server) handleWaive(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	obligationID, err := request.RequireString("obligation_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rationale, err := request.RequireString("rationale")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := s.service.Waive(ctx, sessionID, obligationID, rationale)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(res)
}

func (s *Server) handleAttest(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	obligationID, err := request.RequireString("obligation_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	evidence, err := request.RequireString("evidence")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := s.service.Attest(ctx, sessionID, obligationID, evidence)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(res)
}

func (s *Server) handlePlan(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var items []session.PlanItem
	if raw, ok := request.GetArguments()["items"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(b, &items)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("items must be a list of {rule_id, vars} objects with string vars: %v", err)), nil
		}
	}
	declineReason := request.GetString("decline_reason", "")

	res, err := s.service.Plan(ctx, sessionID, items, declineReason)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(res)
}

// jsonResult returns v as an indented JSON tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleCompleteness(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	report, err := s.service.Completeness(ctx, sessionID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) handleVerify(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := request.RequireString("session_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	report, err := s.service.Verify(ctx, sessionID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(report)
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
