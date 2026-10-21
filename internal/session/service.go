package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ClickUpTaskInfo struct {
	ID          string
	Name        string
	Description string
	ListID      string
	// StatusType is the type of the task's current status: open, custom,
	// done, or closed.
	StatusType string
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
	store        SessionStore
	git          GitCapturer
	clickup      ClickUpClient         // nil-safe: check before use
	completeness CompletenessEvaluator // nil-safe: nil turns completeness off
	gate         GateMode              // configured completion gate; "" = DefaultGate
}

func NewService(store SessionStore, git GitCapturer, clickup ClickUpClient) *Service {
	return &Service{
		store:   store,
		git:     git,
		clickup: clickup,
	}
}

// WithCompleteness sets the evaluator that checks sessions against the
// project's completeness manifest. Without one (or with nil), completeness is
// off and sessions behave as if no project had a manifest.
func (s *Service) WithCompleteness(e CompletenessEvaluator) *Service {
	s.completeness = e
	return s
}

// CheckpointResult is a checkpoint plus the completeness changes it found.
// The checkpoint's fields stay at the top level of its JSON.
type CheckpointResult struct {
	*Checkpoint
	Completeness *CheckpointCompleteness `json:"completeness,omitempty"`
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
	// Completeness is omitted when the project has no manifest.
	Completeness *ContractCompleteness `json:"completeness,omitempty"`
}

type TaskContractInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ContractHistory struct {
	Checkpoints        []CheckpointContractInfo `json:"checkpoints"`
	Snags              []SnagContractInfo       `json:"snags"`
	Decisions          []DecisionContractInfo   `json:"decisions"`
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

type DecisionContractInfo struct {
	Topic     string    `json:"topic"`
	Decision  string    `json:"decision"`
	Rationale string    `json:"rationale"`
	Status    string    `json:"status"`
	Files     []string  `json:"files"`
	CreatedAt time.Time `json:"created_at"`
}

type RelatedResolution struct {
	ErrorSignature string    `json:"error_signature"`
	Resolution     string    `json:"resolution"`
	SourceProject  string    `json:"source_project"`
	ResolvedAt     time.Time `json:"resolved_at"`
}

func (s *Service) enqueueSync(ctx context.Context, taskID, operation string, payloadVal interface{}) {
	s.queueSync(ctx, taskID, operation, payloadVal, s.store.EnqueueSync)
}

// enqueueSyncState queues an op whose payload is the full state its operation
// applies, replacing the payload of the task's op still waiting to run, if
// any (SessionStore.CoalesceSyncOp).
func (s *Service) enqueueSyncState(ctx context.Context, taskID, operation string, payloadVal interface{}) {
	s.queueSync(ctx, taskID, operation, payloadVal, s.store.CoalesceSyncOp)
}

func (s *Service) queueSync(ctx context.Context, taskID, operation string, payloadVal interface{}, enqueue func(context.Context, SyncOp) error) {
	b, err := json.Marshal(payloadVal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to marshal sync payload (task: %s, op: %s): %v\n", taskID, operation, err)
		return
	}
	op := SyncOp{
		Operation:  operation,
		TaskID:     taskID,
		Payload:    string(b),
		MaxRetries: 5,
		CreatedAt:  time.Now(),
	}
	if err := enqueue(ctx, op); err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to enqueue sync op (task: %s, op: %s): %v\n", taskID, operation, err)
	}
}

// enqueueStatus queues a ClickUp status change for a lifecycle role (start,
// reopen, complete, review, archive). The sync worker maps the role onto a
// status of the task's list, so no status names or types are decided here.
// targetStatus is an explicit status the caller asked for on stop, which the
// worker resolves for the role (falling back to the role when it can't).
func (s *Service) enqueueStatus(ctx context.Context, taskID string, role StatusRole, targetStatus string) {
	s.enqueueSync(ctx, taskID, "UPDATE_STATUS", struct {
		Role         StatusRole `json:"role"`
		TargetStatus string     `json:"target_status,omitempty"`
	}{Role: role, TargetStatus: targetStatus})
}

// ErrTargetTaskStatus means a stop named a ClickUp target status it can't
// apply: only a stop that completes the session moves the task.
var ErrTargetTaskStatus = errors.New("target task status only applies when completing a session")

// normalizeStopTarget maps a caller's target status string (from CLI or MCP)
// to the session Status and an optional StatusRole override: RoleReview for
// "in_review"/"review", which completes the session but moves the task to
// review. "abandoned"/"cancelled" are spellings of ARCHIVED, whose role
// (archive) needs no override.
func normalizeStopTarget(targetStatus string) (Status, StatusRole) {
	switch strings.ToUpper(strings.TrimSpace(targetStatus)) {
	case "", "COMPLETED", "COMPLETE", "DONE":
		return StatusCompleted, ""
	case "IN_REVIEW", "IN REVIEW", "REVIEW":
		return StatusCompleted, RoleReview
	case "PAUSED", "PAUSE":
		return StatusPaused, ""
	case "ARCHIVED", "ARCHIVE", "ABANDONED", "ABANDON", "CANCELLED", "CANCELED", "CANCEL":
		return StatusArchived, ""
	default:
		return Status(targetStatus), ""
	}
}

// stopStatusRole returns the ClickUp status role for a Stop transition, or ""
// when the task's ClickUp status should be left alone. No-op transitions
// (from == to) and pausing never change the task's status: pausing leaves the
// task wherever it is in ClickUp (including any manual move a teammate made
// during the session), and only Service.Start sends RoleStart/RoleReopen.
func stopStatusRole(from, to Status) StatusRole {
	if from == to {
		return ""
	}
	switch to {
	case StatusActive, StatusPaused:
		return ""
	case StatusCompleted:
		return RoleComplete
	case StatusArchived:
		// Archiving an already-completed session only tidies local history;
		// it must not move a finished task to the list's closed status.
		if from == StatusCompleted {
			return ""
		}
		return RoleArchive
	}
	return ""
}

func (s *Service) Start(ctx context.Context, taskID, projectPath string, autoGit bool) (*StartResult, error) {
	// 1. Advisory lock check
	lockInfo, err := ReadLockInfo(projectPath)
	if err == nil && lockInfo != nil && lockInfo.SessionID != "" {
		isStale := false
		if lockInfo.PID > 0 && !IsProcessAlive(lockInfo.PID) {
			isStale = true
		}

		if !isStale {
			prevSess, err := s.store.GetSession(ctx, lockInfo.SessionID)
			if err == nil && prevSess != nil && prevSess.Status == StatusActive && prevSess.TaskID != taskID {
				return nil, fmt.Errorf("An active session already exists in %s (session %s). Stop or pause it first.", projectPath, lockInfo.SessionID)
			}
		}
	}

	// 2. ClickUp validation
	taskName := "Local Task"
	taskDesc := ""
	// Starting work on a finished task is a reopen: unlike start, it may move
	// the task out of a done/closed ClickUp status. Whether it is finished is
	// read from ClickUp, since this DB may not know the task's history (a new
	// machine, or a task someone else closed); without a client, a finished
	// local session for the task stands in for it.
	role := RoleStart
	if s.clickup != nil {
		taskInfo, err := s.clickup.GetTask(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("failed to validate task via ClickUp: %w", err)
		}
		taskName = taskInfo.Name
		taskDesc = taskInfo.Description
		if taskInfo.StatusType == "done" || taskInfo.StatusType == "closed" {
			role = RoleReopen
		}
	}

	// 3. Check for existing session
	existing, err := s.store.GetSessionByTaskID(ctx, taskID)
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		return nil, fmt.Errorf("failed to query existing session: %w", err)
	}
	var sessionID string
	var isResumed bool
	var sess *Session
	var inheritedFrom string // the finished session a new one took over from

	if existing != nil {
		if existing.Status == StatusActive {
			sessionID = existing.ID
			existing.TaskName = taskName
			existing.UpdatedAt = time.Now()
			sess = existing
			isResumed = true
			if taskName != "" {
				_ = s.store.UpdateSessionTaskName(ctx, sess.ID, taskName)
			}
		} else if existing.Status == StatusPaused {
			if err := s.store.UpdateSessionStatus(ctx, existing.ID, StatusActive); err != nil {
				return nil, fmt.Errorf("failed to resume session status: %w", err)
			}
			sessionID = existing.ID
			existing.Status = StatusActive
			existing.TaskName = taskName
			existing.UpdatedAt = time.Now()
			sess = existing
			isResumed = true
			if taskName != "" {
				_ = s.store.UpdateSessionTaskName(ctx, sess.ID, taskName)
			}
		} else {
			// Completed or Archived -> new session
			if s.clickup == nil {
				role = RoleReopen
			}
			sessionID = uuid.New().String()
			sess = &Session{
				ID:          sessionID,
				TaskID:      taskID,
				TaskName:    taskName,
				ProjectPath: projectPath,
				Status:      StatusActive,
				PlanState:   inheritedPlanState(existing),
				StartedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			}
			if err := s.store.CreateSession(ctx, sess); err != nil {
				return nil, fmt.Errorf("failed to create new session: %w", err)
			}
			s.recordBaseSHA(ctx, sess)
			s.inheritObligations(ctx, existing, sess)
			inheritedFrom = existing.ID
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
			PlanState:   PlanNone,
			StartedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := s.store.CreateSession(ctx, sess); err != nil {
			return nil, fmt.Errorf("failed to create session: %w", err)
		}
		s.recordBaseSHA(ctx, sess)
	}

	// 4. Capture git context
	var gitCtx *GitContext
	if autoGit && s.git != nil {
		gitCtx, err = s.git.Capture(projectPath)
		if err != nil {
			if !errors.Is(err, ErrNotAGitRepository) {
				fmt.Fprintf(os.Stderr, "[session] git capture warning in %s: %v\n", projectPath, err)
			}
		} else if gitCtx != nil {
			sess.GitBranch = gitCtx.Branch
			_ = s.store.UpdateSessionGitBranch(ctx, sess.ID, gitCtx.Branch)
		}
	}

	// 5. Write lock
	if err := WriteLock(projectPath, sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "[session] warning: failed to write session lock in %s: %v\n", projectPath, err)
	}

	s.enqueueStatus(ctx, sess.TaskID, role, "")

	// 6. Build contract
	contract, err := s.BuildAgenticContract(ctx, sess, gitCtx, isResumed)
	if err != nil {
		return nil, fmt.Errorf("failed to build agentic contract: %w", err)
	}
	contract.Task.Description = taskDesc
	if st := s.evaluateCompleteness(ctx, sess, true); st != nil {
		contract.Completeness = s.startCompleteness(ctx, sess, st, inheritedFrom, taskName, taskDesc)
	}

	return &StartResult{
		Session:         sess,
		GitContext:      gitCtx,
		AgenticContract: contract,
	}, nil
}

func (s *Service) Checkpoint(ctx context.Context, sessionID, summary string, files []string, autoGit bool) (*CheckpointResult, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session for checkpoint: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session for checkpoint: %w", ErrSessionNotFound)
	}

	if sess.Status != StatusActive {
		return nil, fmt.Errorf("cannot create checkpoint for non-active session (status: %s)", sess.Status)
	}

	st := s.evaluateCompleteness(ctx, sess, true)
	cp, err := s.createCheckpoint(ctx, sess, summary, files, autoGit, st)
	if err != nil {
		return nil, err
	}
	res := &CheckpointResult{Checkpoint: cp}
	if st != nil {
		res.Completeness = st.checkpointView()
		if sess.PlanState == PlanSuggested && st.errText() == "" {
			res.Completeness.PlanReminder = planReminder(st.eval.Rules)
		}
		s.remind(ctx, st)
	}
	return res, nil
}

// startCompleteness builds the start contract's completeness section:
// inherited obligations when the session took over from inheritedFrom, and a
// suggested plan when the task names work a plannable rule covers and the
// session has no plan yet.
func (s *Service) startCompleteness(ctx context.Context, sess *Session, st *completenessState, inheritedFrom, taskName, taskDesc string) *ContractCompleteness {
	cc := st.contractView(s.gateFor(st))
	if inheritedFrom != "" {
		var inherited []*Obligation
		for _, o := range st.visible() {
			if o.InheritedFrom != "" {
				inherited = append(inherited, o)
			}
		}
		cc.Inherited = st.views(inherited, true)
		for i := range cc.Inherited {
			cc.Inherited[i].FromSession = inheritedFrom
		}
	}
	noPlan := sess.PlanState == PlanNone || sess.PlanState == PlanSuggested || sess.PlanState == ""
	switch {
	case noPlan && hasPlanned(st.visible()):
		// Planned obligations (e.g. inherited) mean a plan already exists:
		// record it rather than suggesting another.
		s.setPlanState(ctx, sess, PlanPlanned)
	case noPlan && st.errText() == "":
		if sp := suggestPlan(st.eval.Rules, taskName, taskDesc); sp != nil {
			cc.SuggestedPlan = sp
			if sess.PlanState != PlanSuggested {
				s.setPlanState(ctx, sess, PlanSuggested)
			}
		}
	}
	s.remind(ctx, st)
	return cc
}

// hasPlanned reports whether any of obligations came from a plan.
func hasPlanned(obligations []*Obligation) bool {
	for _, o := range obligations {
		if o.Planned {
			return true
		}
	}
	return false
}

// setPlanState saves sess's plan state, logging (not failing) on error.
func (s *Service) setPlanState(ctx context.Context, sess *Session, state PlanState) {
	if err := s.store.SetSessionPlanState(ctx, sess.ID, state); err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to save plan state for session %s: %v\n", sess.ID, err)
		return
	}
	sess.PlanState = state
}

// createCheckpoint saves a checkpoint and queues its progress comment, with
// a completeness line when st is non-nil.
func (s *Service) createCheckpoint(ctx context.Context, sess *Session, summary string, files []string, autoGit bool, st *completenessState) (*Checkpoint, error) {
	var gitCtx *GitContext
	if autoGit && s.git != nil {
		var err error
		gitCtx, err = s.git.Capture(sess.ProjectPath)
		if err != nil && !errors.Is(err, ErrNotAGitRepository) {
			fmt.Fprintf(os.Stderr, "[session] git checkpoint capture warning in %s: %v\n", sess.ProjectPath, err)
		}
	}

	cp := &Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
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

	_ = s.store.UpdateSessionUpdatedAt(ctx, sess.ID, time.Now())

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
	if st != nil {
		commentText += "\n" + st.commentLine()
	}
	s.enqueueSync(ctx, sess.TaskID, "POST_COMMENT", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: commentText})

	return cp, nil
}

// recordBaseSHA stores the commit a new session's changes are measured from.
// It is only called when a session is created, so resuming never moves it.
func (s *Service) recordBaseSHA(ctx context.Context, sess *Session) {
	if s.completeness == nil {
		return
	}
	sha, err := s.completeness.BaseSHA(sess.ProjectPath)
	if err != nil {
		if !errors.Is(err, ErrNotAGitRepository) {
			fmt.Fprintf(os.Stderr, "[session] could not record base commit in %s: %v\n", sess.ProjectPath, err)
		}
		return
	}
	if err := s.store.SetSessionBaseSHA(ctx, sess.ID, sha); err != nil {
		fmt.Fprintf(os.Stderr, "[session] failed to save base commit for session %s: %v\n", sess.ID, err)
		return
	}
	sess.BaseSHA = sha
}

func (s *Service) LogSnag(ctx context.Context, sessionID, errorText, category, resolution string) (*SnagResult, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session to log snag: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session to log snag: %w", ErrSessionNotFound)
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

func (s *Service) LogDecision(ctx context.Context, sessionID, topic, decision, rationale, status string, files []string) (*Decision, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session to log decision: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session to log decision: %w", ErrSessionNotFound)
	}

	if sess.Status != StatusActive {
		return nil, fmt.Errorf("cannot log decision for non-active session (status: %s)", sess.Status)
	}
	return s.createDecision(ctx, sess, topic, decision, rationale, status, files)
}

// createDecision saves a decision for sess and queues its ClickUp comment.
// Unlike LogDecision it doesn't require an active session, so stopping a
// paused session can record a forced completion.
func (s *Service) createDecision(ctx context.Context, sess *Session, topic, decision, rationale, status string, files []string) (*Decision, error) {
	if status == "" {
		status = "no_change"
	}
	sessionID := sess.ID

	d := &Decision{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Topic:     topic,
		Decision:  decision,
		Rationale: rationale,
		Status:    status,
		Files:     files,
		CreatedAt: time.Now(),
	}

	if err := s.store.CreateDecision(ctx, d); err != nil {
		return nil, fmt.Errorf("failed to create decision: %w", err)
	}

	_ = s.store.UpdateSessionUpdatedAt(ctx, sessionID, time.Now())

	var filesStr = "none"
	if len(d.Files) > 0 {
		filesStr = strings.Join(d.Files, ", ")
	}
	commentText := fmt.Sprintf("## 💡 Decision & Rationale Logged: %s\n**Topic:** %s\n**Decision:** %s\n**Outcome:** %s\n**Rationale:** %s\n\n**Related Path(s):** %s",
		d.CreatedAt.Format(time.RFC3339),
		d.Topic,
		d.Decision,
		d.Status,
		d.Rationale,
		filesStr,
	)
	s.enqueueSync(ctx, sess.TaskID, "POST_DECISION", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: commentText})

	return d, nil
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

// Stop ends or pauses a session. Completing a session with required
// completeness obligations open goes through the completion gate (see
// GateMode): under block it returns an *IncompleteSessionError (or
// ErrCompletenessUnchecked when completeness can't be checked) and writes
// nothing, not even the stop's evaluation of the obligations or their
// reminder counts, unless force is set (a human override, recorded as a
// decision). Pausing and archiving are never gated.
func (s *Service) Stop(ctx context.Context, sessionID, summary, targetStatus string, autoGit, force bool) (*StopResult, error) {
	return s.StopWithTaskStatus(ctx, sessionID, summary, targetStatus, "", autoGit, force)
}

// StopWithTaskStatus is Stop with an explicit ClickUp status for the linked
// task (targetTaskStatus, "" for none), which the sync worker resolves for
// the stop's role (see clickup.ResolveTargetStatus). Only a stop that
// completes the session takes one (else ErrTargetTaskStatus, before anything
// is written), and under the review gate it can't move the task to a done or
// closed status.
func (s *Service) StopWithTaskStatus(ctx context.Context, sessionID, summary, targetStatus, targetTaskStatus string, autoGit, force bool) (*StopResult, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("failed to get session: %w", ErrSessionNotFound)
	}

	tStatus, roleOverride := normalizeStopTarget(targetStatus)

	if err := ValidateTransition(sess.Status, tStatus); err != nil {
		return nil, fmt.Errorf("invalid transition: %w", err)
	}

	fromStatus := sess.Status
	role := stopStatusRole(fromStatus, tStatus)
	if roleOverride != "" && role != "" {
		role = roleOverride
	}
	// A target status rides on the completing role, which the gate below
	// decides; any other stop either leaves the task alone or archives it,
	// and a target there would be ignored or get the task past the gate.
	targetTaskStatus = strings.TrimSpace(targetTaskStatus)
	if targetTaskStatus != "" && (tStatus != StatusCompleted || role == "") {
		return nil, fmt.Errorf("%w (stopping %s as %s)", ErrTargetTaskStatus, sess.Status, tStatus)
	}

	// A finished session's obligations are final; only evaluate work that
	// was still in progress. The evaluation is merged in memory only, and
	// written once the gate lets the stop go ahead. Completing it runs the
	// command checks first, when they're enabled; a refused stop keeps
	// nothing of them but what the error reports.
	var st *completenessState
	var pending *mergeResult
	var evalErr error
	if sess.Status == StatusActive || sess.Status == StatusPaused {
		st, pending, evalErr = s.evaluateUnsaved(ctx, sess, tStatus == StatusCompleted)
		if evalErr != nil {
			fmt.Fprintf(os.Stderr, "[session] completeness evaluation failed for session %s: %v\n", sess.ID, evalErr)
		}
	}

	res := &StopResult{SessionID: sessionID, Status: tStatus}
	// The gate runs before anything is written, so a blocked stop leaves the
	// session as it was, its obligations and their reminder counts included.
	if tStatus == StatusCompleted {
		var gate *GateResult
		var gateRole StatusRole
		var err error
		switch {
		case st != nil:
			gate, gateRole, err = s.applyGate(ctx, sess, st, force)
		case evalErr != nil:
			// The manifest (and any gate it sets) couldn't be read, so the
			// configured gate decides, acting as if work may be open.
			gate, gateRole, err = s.uncheckedGate(ctx, sess, s.configuredGate(), "evaluation failed: "+evalErr.Error(), nil, force)
		}
		if err != nil {
			return nil, err
		}
		res.Gate = gate
		if gateRole != "" && role != "" {
			// The target goes with the review role, which the worker never
			// lets move the task to a done or closed status, so a target
			// that is itself a review/QA stage is still honored.
			role = gateRole
		}
	}

	// The stop goes ahead: record its evaluation. An obligation another
	// writer inserted first keeps the stored ID, so list the gate's open
	// obligations again by the IDs written.
	_ = s.persistMerge(ctx, sess, st, pending)
	if g := res.Gate; g != nil && g.Unchecked == "" && len(g.OpenRequired) > 0 {
		g.OpenRequired = st.views(st.openRequired(), false)
	}

	// The completeness table goes in the Session Finished comment below, so
	// the stop checkpoint's comment leaves out its summary line.
	if summary != "" {
		_, err := s.createCheckpoint(ctx, sess, summary, nil, autoGit, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to write stop checkpoint: %w", err)
		}
	}

	if err := s.store.UpdateSessionStatus(ctx, sessionID, tStatus); err != nil {
		return nil, fmt.Errorf("failed to update session status: %w", err)
	}

	if tStatus != StatusPaused {
		_ = RemoveLock(sess.ProjectPath)
	}

	// 1. Enqueue status update
	if role != "" {
		s.enqueueStatus(ctx, sess.TaskID, role, targetTaskStatus)
	}

	// 2. Enqueue session summary comment
	dbCheckpoints, _ := s.store.ListCheckpoints(ctx, sessionID, 0)
	dbSnags, _ := s.store.ListSnagsBySession(ctx, sessionID)
	dbDecisions, _ := s.store.ListDecisionsBySession(ctx, sessionID)
	duration := time.Since(sess.StartedAt).Round(time.Second)

	commentText := fmt.Sprintf("## Session Finished: %s\n**Status:** %s\n**Duration:** %s\n**Checkpoints:** %d\n**Snags:** %d\n**Decisions Logged:** %d",
		time.Now().Format(time.RFC3339),
		tStatus,
		duration.String(),
		len(dbCheckpoints),
		len(dbSnags),
		len(dbDecisions),
	)
	if res.Gate != nil {
		if line := res.Gate.commentLine(); line != "" {
			commentText += "\n" + line
		}
	}
	if st != nil {
		commentText += "\n\n" + st.reportTable()
		res.Completeness = st.report(sessionID)
	}
	s.enqueueSync(ctx, sess.TaskID, "POST_COMMENT", struct {
		CommentText string `json:"comment_text"`
	}{CommentText: commentText})

	return res, nil
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

	dbDecisions, err := s.store.ListDecisionsBySession(ctx, sess.ID)
	var decisions []DecisionContractInfo
	if err == nil {
		for _, d := range dbDecisions {
			decisions = append(decisions, DecisionContractInfo{
				Topic:     d.Topic,
				Decision:  d.Decision,
				Rationale: d.Rationale,
				Status:    d.Status,
				Files:     d.Files,
				CreatedAt: d.CreatedAt,
			})
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
			Decisions:          decisions,
			RelatedResolutions: relatedResolutions,
		},
		Resumed: resumed,
	}

	return contract, nil
}
