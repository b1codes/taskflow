package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ClickUpTaskInfo struct {
	ID          string
	Name        string
	Description string
	ListID      string
}

type ClickUpStatusInfo struct {
	Status     string
	Type       string
	OrderIndex int
}

type ClickUpClient interface {
	GetTask(ctx context.Context, taskID string) (*ClickUpTaskInfo, error)
	GetListStatuses(ctx context.Context, listID string) ([]ClickUpStatusInfo, error)
}

type GitCapturer interface {
	Capture(projectPath string) (*GitContext, error)
}

type Service struct {
	store   SessionStore
	git     GitCapturer
	clickup ClickUpClient // nil-safe: check before use
}

func NewService(store SessionStore, git GitCapturer, clickup ClickUpClient) *Service {
	return &Service{
		store:   store,
		git:     git,
		clickup: clickup,
	}
}

type StartResult struct {
	Session         *Session         `json:"session"`
	GitContext      *GitContext      `json:"git"`
	AgenticContract *AgenticContract `json:"contract"`
}

type SnagResult struct {
	Snag               *Snag               `json:"snag"`
	RelatedResolutions []RelatedResolution `json:"related_resolutions"`
}

type SessionSummary struct {
	SessionID           string    `json:"session_id"`
	TaskID              string    `json:"task_id"`
	TaskName            string    `json:"task_name"`
	ProjectPath         string    `json:"project_path"`
	Status              Status    `json:"status"`
	LastCheckpointAt    time.Time `json:"last_checkpoint_at"`
	UnresolvedSnagCount int       `json:"unresolved_snag_count"`
	PendingSyncCount    int       `json:"pending_sync_count"`
}

type AgenticContract struct {
	SessionID string           `json:"session_id"`
	Task      TaskContractInfo `json:"task"`
	Git       *GitContext      `json:"git"`
	History   ContractHistory  `json:"history"`
	Resumed   bool             `json:"resumed"`
}

type TaskContractInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ContractHistory struct {
	Checkpoints        []CheckpointContractInfo `json:"checkpoints"`
	Snags              []SnagContractInfo       `json:"snags"`
	RelatedResolutions []RelatedResolution      `json:"related_resolutions"`
}

type CheckpointContractInfo struct {
	Summary   string    `json:"summary"`
	Files     []string  `json:"files"`
	CreatedAt time.Time `json:"created_at"`
}

type SnagContractInfo struct {
	Error      string    `json:"error"`
	Category   string    `json:"category"`
	Resolution string    `json:"resolution"`
	CreatedAt  time.Time `json:"created_at"`
}

type RelatedResolution struct {
	ErrorSignature string    `json:"error_signature"`
	Resolution     string    `json:"resolution"`
	SourceProject  string    `json:"source_project"`
	ResolvedAt     time.Time `json:"resolved_at"`
}

func (s *Service) enqueueSync(ctx context.Context, taskID, operation string, payloadVal interface{}) {
	b, err := json.Marshal(payloadVal)
	if err != nil {
		return
	}
	op := SyncOp{
		Operation:  operation,
		TaskID:     taskID,
		Payload:    string(b),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	_ = s.store.EnqueueSync(ctx, op)
}

func (s *Service) Start(ctx context.Context, taskID, projectPath string, autoGit bool) (*StartResult, error) {
	// 1. Advisory lock check
	lockSessionID, err := ReadLock(projectPath)
	if err == nil && lockSessionID != "" {
		prevSess, err := s.store.GetSession(ctx, lockSessionID)
		if err == nil && prevSess != nil && prevSess.Status == StatusActive && prevSess.TaskID != taskID {
			return nil, fmt.Errorf("An active session already exists in %s (session %s). Stop or pause it first.", projectPath, lockSessionID)
		}
	}

	// 2. ClickUp validation
	taskName := "Local Task"
	taskDesc := ""
	if s.clickup != nil {
		taskInfo, err := s.clickup.GetTask(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("failed to validate task via ClickUp: %w", err)
		}
		taskName = taskInfo.Name
		taskDesc = taskInfo.Description
	}

	// 3. Check for existing session
	existing, err := s.store.GetSessionByTaskID(ctx, taskID)
	var sessionID string
	var isResumed bool
	var sess *Session

	if err == nil && existing != nil {
		if existing.Status == StatusActive {
			sessionID = existing.ID
			sess = existing
		} else if existing.Status == StatusPaused {
			if err := s.store.UpdateSessionStatus(ctx, existing.ID, StatusActive); err != nil {
				return nil, fmt.Errorf("failed to resume session status: %w", err)
			}
			sessionID = existing.ID
			existing.Status = StatusActive
			existing.UpdatedAt = time.Now()
			sess = existing
			isResumed = true
		} else {
			// Completed or Archived -> new session
			sessionID = uuid.New().String()
			sess = &Session{
				ID:          sessionID,
				TaskID:      taskID,
				TaskName:    taskName,
				ProjectPath: projectPath,
				Status:      StatusActive,
				StartedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			}
			if err := s.store.CreateSession(ctx, sess); err != nil {
				return nil, fmt.Errorf("failed to create new session: %w", err)
			}
		}
	} else {
		// New session
		sessionID = uuid.New().String()
		sess = &Session{
			ID:          sessionID,
			TaskID:      taskID,
			TaskName:    taskName,
			ProjectPath: projectPath,
			Status:      StatusActive,
			StartedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := s.store.CreateSession(ctx, sess); err != nil {
			return nil, fmt.Errorf("failed to create session: %w", err)
		}
	}

	// 4. Capture git context
	var gitCtx *GitContext
	if autoGit && s.git != nil {
		gitCtx, err = s.git.Capture(projectPath)
		if err == nil && gitCtx != nil {
			sess.GitBranch = gitCtx.Branch
			_ = s.store.UpdateSessionGitBranch(ctx, sess.ID, gitCtx.Branch)
		}
	}

	// 5. Write lock
	_ = WriteLock(projectPath, sessionID)

	s.enqueueSync(ctx, sess.TaskID, "UPDATE_STATUS", struct {
		StatusType string `json:"status_type"`
	}{StatusType: "active"})

	// 6. Build contract
	contract, err := s.BuildAgenticContract(ctx, sess, gitCtx, isResumed)
	if err != nil {
		return nil, fmt.Errorf("failed to build agentic contract: %w", err)
	}
	contract.Task.Description = taskDesc

	return &StartResult{
		Session:         sess,
		GitContext:      gitCtx,
		AgenticContract: contract,
	}, nil
}

func (s *Service) Checkpoint(ctx context.Context, sessionID, summary string, files []string, autoGit bool) (*Checkpoint, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session for checkpoint: %w", err)
	}

	if sess.Status != StatusActive {
		return nil, fmt.Errorf("cannot create checkpoint for non-active session (status: %s)", sess.Status)
	}

	var gitCtx *GitContext
	if autoGit && s.git != nil {
		gitCtx, _ = s.git.Capture(sess.ProjectPath)
	}

	cp := &Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Summary:   summary,
		Files:     files,
		CreatedAt: time.Now(),
	}

	if gitCtx != nil {
		cp.GitBranch = gitCtx.Branch
		cp.GitSHA = gitCtx.SHA
		cp.GitDirty = gitCtx.DirtyFiles
	}

	if err := s.store.CreateCheckpoint(ctx, cp); err != nil {
		return nil, fmt.Errorf("failed to save checkpoint: %w", err)
	}

	_ = s.store.UpdateSessionUpdatedAt(ctx, sessionID, time.Now())

	var filesStr = "none"
	if len(cp.Files) > 0 {
		filesStr = strings.Join(cp.Files, ", ")
	}
	commentText := fmt.Sprintf("## Progress Checkpoint: %s\n%s\n\n**Files:** %s\n**Branch:** %s @ %s",
		cp.CreatedAt.Format(time.RFC3339),
		cp.Summary,
		filesStr,
		cp.GitBranch,
		cp.GitSHA,
	)
	s.enqueueSync(ctx, sess.TaskID, "POST_COMMENT", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: commentText})

	return cp, nil
}

func (s *Service) LogSnag(ctx context.Context, sessionID, errorText, category, resolution string) (*SnagResult, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session to log snag: %w", err)
	}

	sig := NormalizeErrorSignature(errorText)
	snag := &Snag{
		ID:             uuid.New().String(),
		SessionID:      sessionID,
		Error:          errorText,
		ErrorSignature: sig,
		Category:       category,
		CreatedAt:      time.Now(),
	}

	if resolution != "" {
		snag.Resolution = resolution
		snag.Resolved = true
		now := time.Now()
		snag.ResolvedAt = &now
	}

	if err := s.store.CreateSnag(ctx, snag); err != nil {
		return nil, fmt.Errorf("failed to create snag: %w", err)
	}

	_ = s.store.UpdateSessionUpdatedAt(ctx, sessionID, time.Now())

	snagComment := fmt.Sprintf("## Snag Logged: %s\n**Error:** %s\n**Category:** %s",
		snag.CreatedAt.Format(time.RFC3339),
		snag.Error,
		snag.Category,
	)
	if snag.Resolution != "" {
		snagComment += fmt.Sprintf("\n**Resolution:** %s", snag.Resolution)
	}
	s.enqueueSync(ctx, sess.TaskID, "POST_SNAG", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: snagComment})

	// Find matching resolutions
	var related []RelatedResolution
	matches, err := s.store.FindMatchingResolutions(ctx, sig)
	if err == nil {
		for _, m := range matches {
			sourceProject := ""
			sInfo, err := s.store.GetSession(ctx, m.SessionID)
			if err == nil && sInfo != nil {
				sourceProject = sInfo.ProjectPath
			}

			var resolvedAt time.Time
			if m.ResolvedAt != nil {
				resolvedAt = *m.ResolvedAt
			}

			related = append(related, RelatedResolution{
				ErrorSignature: m.ErrorSignature,
				Resolution:     m.Resolution,
				SourceProject:  sourceProject,
				ResolvedAt:     resolvedAt,
			})
		}
	}

	return &SnagResult{
		Snag:               snag,
		RelatedResolutions: related,
	}, nil
}

func (s *Service) List(ctx context.Context, filter SessionFilter) ([]*SessionSummary, error) {
	sessions, err := s.store.ListSessions(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query sessions: %w", err)
	}

	var summaries []*SessionSummary
	for _, sess := range sessions {
		// Count unresolved snags
		snags, _ := s.store.ListSnagsBySession(ctx, sess.ID)
		unresolvedCount := 0
		for _, snag := range snags {
			if !snag.Resolved {
				unresolvedCount++
			}
		}

		// Count pending syncs
		pendingCount, _ := s.store.CountPendingByTaskID(ctx, sess.TaskID)

		// Get last checkpoint time
		var lastCheckpointAt time.Time
		cps, _ := s.store.ListCheckpoints(ctx, sess.ID, 1)
		if len(cps) > 0 {
			lastCheckpointAt = cps[0].CreatedAt
		}

		summaries = append(summaries, &SessionSummary{
			SessionID:           sess.ID,
			TaskID:              sess.TaskID,
			TaskName:            sess.TaskName,
			ProjectPath:         sess.ProjectPath,
			Status:              sess.Status,
			LastCheckpointAt:    lastCheckpointAt,
			UnresolvedSnagCount: unresolvedCount,
			PendingSyncCount:    pendingCount,
		})
	}

	return summaries, nil
}

func (s *Service) Stop(ctx context.Context, sessionID, summary, targetStatus string, autoGit bool) error {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	tStatus := Status(targetStatus)
	if tStatus == "" {
		tStatus = StatusCompleted
	}

	if err := ValidateTransition(sess.Status, tStatus); err != nil {
		return fmt.Errorf("invalid transition: %w", err)
	}

	if summary != "" {
		_, err := s.Checkpoint(ctx, sessionID, summary, nil, autoGit)
		if err != nil {
			return fmt.Errorf("failed to write stop checkpoint: %w", err)
		}
	}

	if err := s.store.UpdateSessionStatus(ctx, sessionID, tStatus); err != nil {
		return fmt.Errorf("failed to update session status: %w", err)
	}

	if tStatus != StatusPaused {
		_ = RemoveLock(sess.ProjectPath)
	}

	// 1. Enqueue status update
	statusType := "closed"
	if tStatus == StatusPaused {
		statusType = "active"
	}
	s.enqueueSync(ctx, sess.TaskID, "UPDATE_STATUS", struct {
		StatusType string `json:"status_type"`
	}{StatusType: statusType})

	// 2. Enqueue session summary comment
	dbCheckpoints, _ := s.store.ListCheckpoints(ctx, sessionID, 0)
	dbSnags, _ := s.store.ListSnagsBySession(ctx, sessionID)
	duration := time.Since(sess.StartedAt).Round(time.Second)

	commentText := fmt.Sprintf("## Session Finished: %s\n**Status:** %s\n**Duration:** %s\n**Checkpoints:** %d\n**Snags:** %d",
		time.Now().Format(time.RFC3339),
		tStatus,
		duration.String(),
		len(dbCheckpoints),
		len(dbSnags),
	)
	s.enqueueSync(ctx, sess.TaskID, "POST_COMMENT", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: commentText})

	return nil
}

func (s *Service) BuildAgenticContract(ctx context.Context, sess *Session, gitCtx *GitContext, resumed bool) (*AgenticContract, error) {
	dbCheckpoints, err := s.store.ListCheckpoints(ctx, sess.ID, 3)
	if err != nil {
		return nil, err
	}

	var checkpoints []CheckpointContractInfo
	for _, cp := range dbCheckpoints {
		checkpoints = append(checkpoints, CheckpointContractInfo{
			Summary:   cp.Summary,
			Files:     cp.Files,
			CreatedAt: cp.CreatedAt,
		})
	}

	dbSnags, err := s.store.ListSnagsBySession(ctx, sess.ID)
	if err != nil {
		return nil, err
	}

	var unresolvedSnags []SnagContractInfo
	var relatedResolutions []RelatedResolution

	for _, snag := range dbSnags {
		if !snag.Resolved {
			unresolvedSnags = append(unresolvedSnags, SnagContractInfo{
				Error:      snag.Error,
				Category:   snag.Category,
				Resolution: snag.Resolution,
				CreatedAt:  snag.CreatedAt,
			})

			// Search matching resolutions
			matches, err := s.store.FindMatchingResolutions(ctx, snag.ErrorSignature)
			if err == nil {
				for _, m := range matches {
					sourceProject := ""
					sInfo, err := s.store.GetSession(ctx, m.SessionID)
					if err == nil && sInfo != nil {
						sourceProject = sInfo.ProjectPath
					}

					var resolvedAt time.Time
					if m.ResolvedAt != nil {
						resolvedAt = *m.ResolvedAt
					}

					relatedResolutions = append(relatedResolutions, RelatedResolution{
						ErrorSignature: m.ErrorSignature,
						Resolution:     m.Resolution,
						SourceProject:  sourceProject,
						ResolvedAt:     resolvedAt,
					})
				}
			}
		}
	}

	contract := &AgenticContract{
		SessionID: sess.ID,
		Task: TaskContractInfo{
			ID:   sess.TaskID,
			Name: sess.TaskName,
		},
		Git: gitCtx,
		History: ContractHistory{
			Checkpoints:        checkpoints,
			Snags:              unresolvedSnags,
			RelatedResolutions: relatedResolutions,
		},
		Resumed: resumed,
	}

	return contract, nil
}
