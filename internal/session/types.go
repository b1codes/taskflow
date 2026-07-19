package session

import "time"

type Session struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	TaskName    string    `json:"task_name"`
	ProjectPath string    `json:"project_path"`
	Status      Status    `json:"status"`
	GitBranch   string    `json:"git_branch"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Checkpoint struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Summary   string    `json:"summary"`
	Files     []string  `json:"files"`
	GitBranch string    `json:"git_branch"`
	GitSHA    string    `json:"git_sha"`
	GitDirty  []string  `json:"git_dirty"`
	CreatedAt time.Time `json:"created_at"`
}

type Snag struct {
	ID             string     `json:"id"`
	SessionID      string     `json:"session_id"`
	Error          string     `json:"error"`
	ErrorSignature string     `json:"error_signature"`
	Category       string     `json:"category"`
	Resolution     string     `json:"resolution"`
	Resolved       bool       `json:"resolved"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
}

type GitContext struct {
	Branch     string   `json:"branch"`
	SHA        string   `json:"sha"`
	DirtyFiles []string `json:"dirty_files"`
}

type SessionFilter struct {
	Status      []Status
	ProjectPath string
}

type SyncOp struct {
	ID              int64      `json:"id"`
	Operation       string     `json:"operation"`
	TaskID          string     `json:"task_id"`
	Payload         string     `json:"payload"` // JSON string
	Status          string     `json:"status"`
	Retries         int        `json:"retries"`
	MaxRetries      int        `json:"max_retries"`
	CreatedAt       time.Time  `json:"created_at"`
	LastAttemptedAt *time.Time `json:"last_attempted_at"`
	ErrorMsg        string     `json:"error_msg"`
}
