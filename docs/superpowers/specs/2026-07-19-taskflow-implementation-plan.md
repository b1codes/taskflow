# Taskflow Implementation Plan

**Date:** 2026-07-19
**Spec:** [2026-07-19-taskflow-design.md](./2026-07-19-taskflow-design.md)
**Status:** Ready for execution

---

## How to Read This Plan

Each task is a self-contained unit of work. Tasks within a phase are ordered by dependency — complete them top to bottom. Each task lists:

- **Files** — what to create or modify
- **Depends on** — which prior tasks must be done first
- **Do** — concrete implementation steps
- **Test** — how to verify the task is done
- **Accept** — the "definition of done" for this task

---

## Phase 1: Skeleton

> **Goal:** A binary that boots, creates `~/.taskflow/`, initializes the SQLite database with all tables, and responds to `taskflow init` and `taskflow --help`.

### Task 1.1: Initialize Go Module

**Files:** `go.mod`
**Depends on:** Nothing

**Do:**
1. Run `go mod init github.com/b1codes/taskflow`.
2. Add dependencies:
   - `modernc.org/sqlite`
   - `github.com/spf13/cobra`
   - `github.com/mark3labs/mcp-go`
   - `github.com/google/uuid`
   - `github.com/BurntSushi/toml`
3. Run `go mod tidy`.

**Test:** `go mod verify` exits 0.
**Accept:** `go.mod` and `go.sum` exist with all dependencies resolved.

---

### Task 1.2: Config Package

**Files:** `internal/config/config.go`
**Depends on:** 1.1

**Do:**
1. Define a `Config` struct with sections matching the spec:
   ```go
   type Config struct {
       ClickUp  ClickUpConfig
       Git      GitConfig
       Sync     SyncConfig
       Database DatabaseConfig
   }
   ```
2. Implement `Load() (*Config, error)`:
   - Expand `~/.taskflow/config.toml` path.
   - If file exists, parse with `BurntSushi/toml`.
   - Apply environment variable overrides (`CLICKUP_API_KEY` overrides `ClickUp.APIKey`).
   - Fill remaining fields from compiled defaults.
3. Implement `EnsureDir() error`:
   - Creates `~/.taskflow/` directory if it doesn't exist.
   - If `config.toml` doesn't exist, writes a default one with comments.
4. Implement `DBPath() string` that expands `~` in the database path.

**Test:**
- Unit test: `Load` with no file returns defaults.
- Unit test: `Load` with env var `CLICKUP_API_KEY=test` overrides config file value.
- Unit test: `EnsureDir` creates directory and default config in a temp dir.

**Accept:** Config loads with correct precedence (env > file > default). `~/.taskflow/` is created on demand.

---

### Task 1.3: Store Package — Database Initialization & Migrations

**Files:** `internal/store/store.go`, `internal/store/sqlite.go`, `internal/store/migrations.go`
**Depends on:** 1.1, 1.2

**Do:**
1. Define the `Store` interface in `store.go` (empty for now — methods added in Phase 2):
   ```go
   type Store interface {
       Close() error
   }
   ```
2. Implement `SQLiteStore` in `sqlite.go`:
   - `New(dbPath string) (*SQLiteStore, error)` — opens the database, sets PRAGMAs (WAL mode, busy timeout, foreign keys), runs migrations.
   - Stores `*sql.DB` internally.
3. Implement migrations in `migrations.go`:
   - Use a simple version table (`schema_version`) with an integer version.
   - Migration 1: Create all five tables (`sessions`, `checkpoints`, `snags`, `sync_queue`, `clickup_cache`) and all indexes from the spec.
   - Migrations run sequentially on startup. Each migration is idempotent.
4. PRAGMAs to set on every connection:
   ```sql
   PRAGMA journal_mode=WAL;
   PRAGMA busy_timeout=5000;
   PRAGMA foreign_keys=ON;
   ```

**Test:**
- Unit test: `New(":memory:")` succeeds, all tables exist (query `sqlite_master`).
- Unit test: Running `New` twice on the same file doesn't re-run migrations.
- Unit test: Verify WAL mode is active (`PRAGMA journal_mode` returns `wal`).

**Accept:** `SQLiteStore` opens a database, creates all tables and indexes, and sets WAL mode. Migrations are versioned and idempotent.

---

### Task 1.4: CLI Root Command & Entry Point

**Files:** `cmd/taskflow/main.go`, `internal/cli/root.go`
**Depends on:** 1.2, 1.3

**Do:**
1. `cmd/taskflow/main.go`: Call `cli.Execute()`.
2. `internal/cli/root.go`:
   - Define the root cobra command with app name, version, and description.
   - In `PersistentPreRunE`: call `config.EnsureDir()` and `config.Load()`, store config on the command context.
   - Register `--verbose` flag for debug output.

**Test:** `go build ./cmd/taskflow && ./taskflow --help` prints usage with app description.
**Accept:** Binary compiles, `--help` works, `~/.taskflow/` is created on first run.

---

### Task 1.5: `init` Command

**Files:** `internal/cli/init.go`
**Depends on:** 1.4

**Do:**
1. Register `init` subcommand on the root command.
2. Add `--refresh` boolean flag (used later in Phase 4 for ClickUp cache refresh; for now, accepted but no-op).
3. Behavior:
   - Load config.
   - Call `store.New(config.DBPath())` to initialize the database.
   - Print: `✓ Database initialized at ~/.taskflow/sessions.db`
   - Print table count and schema version for confirmation.
   - Close the store.

**Test:** Run `taskflow init` in a clean environment. Verify `~/.taskflow/sessions.db` is created. Run again — no errors (idempotent).
**Accept:** `taskflow init` creates the database and prints confirmation. Runs cleanly on repeat.

---

### Task 1.6: `serve` Command (Placeholder)

**Files:** `internal/cli/serve.go`
**Depends on:** 1.4

**Do:**
1. Register `serve` subcommand on the root command.
2. For now, print: `MCP server not yet implemented (Phase 3)` and exit 0.
3. This placeholder ensures the command exists in `--help` output and the CLI structure is complete.

**Test:** `taskflow serve` prints the placeholder message.
**Accept:** Command is registered and exits cleanly.

---

### Phase 1 Checkpoint

**Verify all of the following before moving to Phase 2:**
- [ ] `go build ./cmd/taskflow` produces a binary
- [ ] `taskflow --help` shows `init` and `serve` subcommands
- [ ] `taskflow init` creates `~/.taskflow/sessions.db` with all 5 tables + schema_version
- [ ] `taskflow init` is idempotent (runs twice without error)
- [ ] All unit tests pass: `go test ./internal/config/... ./internal/store/...`

---

## Phase 2: Core Sessions

> **Goal:** Fully functional local session tracking via CLI. No MCP, no ClickUp yet. All unit tests passing.

### Task 2.1: Session Domain Types

**Files:** `internal/session/types.go`, `internal/session/status.go`
**Depends on:** Phase 1

**Do:**
1. Define domain types in `types.go`:
   ```go
   type Session struct {
       ID          string
       TaskID      string
       TaskName    string
       ProjectPath string
       Status      Status
       GitBranch   string
       StartedAt   time.Time
       UpdatedAt   time.Time
   }

   type Checkpoint struct {
       ID        string
       SessionID string
       Summary   string
       Files     []string
       GitBranch string
       GitSHA    string
       GitDirty  []string
       CreatedAt time.Time
   }

   type Snag struct {
       ID             string
       SessionID      string
       Error          string
       ErrorSignature string
       Category       string
       Resolution     string
       Resolved       bool
       CreatedAt      time.Time
       ResolvedAt     *time.Time
   }

   type GitContext struct {
       Branch     string
       SHA        string
       DirtyFiles []string
   }
   ```
2. Define `Status` type and constants in `status.go`:
   ```go
   type Status string
   const (
       StatusActive    Status = "ACTIVE"
       StatusPaused    Status = "PAUSED"
       StatusCompleted Status = "COMPLETED"
       StatusArchived  Status = "ARCHIVED"
   )
   ```
3. Add a `Valid() bool` method on `Status`.
4. Add validation functions: `ValidateTransition(from, to Status) error` — enforce allowed transitions (e.g., can't go from COMPLETED to ACTIVE directly).

**Test:**
- Unit test: All status constants are valid.
- Unit test: `ValidateTransition` allows ACTIVE→PAUSED, ACTIVE→COMPLETED, PAUSED→ACTIVE, rejects COMPLETED→ACTIVE.

**Accept:** Domain types compile and represent the full data model. Status transitions are validated.

---

### Task 2.2: Error Signature Normalization

**Files:** `internal/session/signature.go`
**Depends on:** 2.1

**Do:**
1. Implement `NormalizeErrorSignature(error string) string`:
   - Strip absolute file paths: regex replace `/[^\s:]+\.(go|js|py|ts|rs|java|rb|c|cpp|h)` with `<file>`.
   - Strip line numbers: regex replace `:\d+:` with `:<line>:`.
   - Strip UUIDs: regex replace standard UUID pattern with `<uuid>`.
   - Strip ISO timestamps: regex replace `\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}` with `<timestamp>`.
   - Lowercase the result.
   - Collapse multiple whitespace to single space, trim.

**Test:**
- `"/Users/me/projectA/main.go:42: undefined: Foo"` → `"<file>:<line>: undefined: foo"`
- `"/home/dev/handler.go:99: undefined: Foo"` → same signature
- `"error at 2026-07-19T14:00:00: connection refused"` → `"error at <timestamp>: connection refused"`
- Empty string → empty string.

**Accept:** Signature normalization is deterministic and produces stable, matchable keys across different file paths and line numbers.

---

### Task 2.3: Git Context Package

**Files:** `internal/gitctx/gitctx.go`
**Depends on:** 2.1

**Do:**
1. Implement `Capture(projectPath string) (*session.GitContext, error)`:
   - Run `git -C <projectPath> rev-parse --abbrev-ref HEAD` → branch name.
   - Run `git -C <projectPath> rev-parse --short HEAD` → commit SHA.
   - Run `git -C <projectPath> status --porcelain` → parse dirty files.
   - If any git command fails (not a repo, git not installed), return `nil, nil` (non-fatal, per spec).
2. Use `os/exec` to run git commands. Set a 5-second timeout via `context.Context`.

**Test:**
- Unit test with a temp dir initialized via `git init`, with a committed file and an uncommitted file. Verify branch, SHA, and dirty file list.
- Unit test with a non-git temp dir. Verify `nil, nil` returned (graceful fallback).

**Accept:** Git context is captured correctly. Non-git directories return nil without error.

---

### Task 2.4: Store Interface — Session CRUD

**Files:** `internal/store/store.go`, `internal/store/sessions.go`
**Depends on:** 1.3, 2.1

**Do:**
1. Expand the `Store` interface with session methods:
   ```go
   type Store interface {
       // Sessions
       CreateSession(ctx context.Context, s *session.Session) error
       GetSession(ctx context.Context, id string) (*session.Session, error)
       GetSessionByTaskID(ctx context.Context, taskID string) (*session.Session, error)
       GetActiveSessionByProject(ctx context.Context, projectPath string) (*session.Session, error)
       UpdateSessionStatus(ctx context.Context, id string, status session.Status) error
       ListSessions(ctx context.Context, filter SessionFilter) ([]*session.Session, error)

       // Checkpoints
       CreateCheckpoint(ctx context.Context, cp *session.Checkpoint) error
       ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]*session.Checkpoint, error)

       // Snags
       CreateSnag(ctx context.Context, snag *session.Snag) error
       ListSnagsBySession(ctx context.Context, sessionID string) ([]*session.Snag, error)
       FindMatchingResolutions(ctx context.Context, signature string) ([]*session.Snag, error)

       Close() error
   }

   type SessionFilter struct {
       Status      []session.Status
       ProjectPath string
   }
   ```
2. Implement all methods on `SQLiteStore` in `sessions.go`.
3. For `FindMatchingResolutions`: query snags across all sessions where `error_signature = ?` AND `resolved = 1`, ordered by `resolved_at DESC`.
4. For JSON array fields (`files`, `git_dirty`), use `encoding/json` to marshal/unmarshal `[]string` to/from TEXT columns.

**Test:**
- Unit test full CRUD cycle: create session → get by ID → get by task_id → update status → list with filter.
- Unit test checkpoint CRUD: create 5 checkpoints → list with limit 3 → verify order (newest first).
- Unit test snag matching: create snag with resolution in session A → call `FindMatchingResolutions` with same signature → verify it returns.
- Unit test `ListSessions` with status filter and project_path filter.

**Accept:** All store operations work against in-memory SQLite. Cross-project snag matching returns resolved snags from other sessions.

---

### Task 2.5: Session Lock File Management

**Files:** `internal/session/lock.go`
**Depends on:** 2.1

**Do:**
1. Implement `WriteLock(projectPath, sessionID string) error`:
   - Create `.b1codes/` directory in `projectPath` if needed.
   - Write `session.lock` containing the session ID as plain text.
2. Implement `ReadLock(projectPath string) (sessionID string, err error)`:
   - Read `.b1codes/session.lock`. Return empty string if not found.
3. Implement `RemoveLock(projectPath string) error`:
   - Delete `.b1codes/session.lock`. No error if not found.
4. All operations are best-effort — errors are logged but don't block session operations (per spec: lock is advisory).

**Test:**
- Unit test: Write → Read → verify ID matches → Remove → Read returns empty.
- Unit test: Read on nonexistent directory returns empty string, no error.

**Accept:** Lock files are written, read, and removed correctly. Failures are non-fatal.

---

### Task 2.6: Session Service (Orchestration Layer)

**Files:** `internal/session/service.go`
**Depends on:** 2.1, 2.2, 2.3, 2.4, 2.5

**Do:**
1. Define `Service` struct that holds a `store.Store` and a `config.Config`:
   ```go
   type Service struct {
       store  store.Store
       config *config.Config
   }
   ```
2. Implement `Start(ctx context.Context, taskID, projectPath string) (*StartResult, error)`:
   - Check for active lock file in `projectPath`. If found and points to an ACTIVE session, return error.
   - Query store for existing session with `taskID`:
     - ACTIVE → return it (idempotent).
     - PAUSED → resume (update status to ACTIVE).
     - COMPLETED/ARCHIVED → create new session.
     - None → create new session.
   - Capture git context if `config.Git.AutoContext` is true.
   - Write lock file.
   - Assemble and return `StartResult` (session + git context + history).
3. Implement `Checkpoint(ctx context.Context, sessionID, summary string, files []string) (*session.Checkpoint, error)`:
   - Validate session is ACTIVE.
   - Capture git context.
   - Create checkpoint in store.
   - Update session `updated_at`.
4. Implement `LogSnag(ctx context.Context, sessionID, errorText, category, resolution string) (*SnagResult, error)`:
   - Normalize error signature.
   - Create snag in store.
   - Query for matching resolutions.
   - Return snag + matching resolutions.
5. Implement `List(ctx context.Context, filter store.SessionFilter) ([]*SessionSummary, error)`:
   - Query store.
   - For each session, count unresolved snags.
6. Implement `Stop(ctx context.Context, sessionID, summary, targetStatus string) error`:
   - If summary provided, create final checkpoint.
   - Validate and apply status transition.
   - Remove lock file (unless PAUSED).
7. Implement `BuildAgenticContract(ctx context.Context, s *session.Session) (*AgenticContract, error)`:
   - Load last 3 checkpoints.
   - Load all unresolved snags for this task.
   - For each unresolved snag, find cross-project matching resolutions.
   - Assemble the JSON-serializable contract struct.

**Test:**
- Unit test full start → checkpoint → snag → stop lifecycle using in-memory store.
- Unit test idempotent start (calling Start twice with same task_id returns same session).
- Unit test resume flow (Start → Stop with PAUSED → Start again resumes).
- Unit test agentic contract assembly (verify last 3 checkpoints, snags, cross-project resolutions).
- Unit test lock file conflict detection.

**Accept:** Service orchestrates all session lifecycle operations. Agentic contract is correctly assembled with history.

---

### Task 2.7: CLI Commands — `start`, `stop`, `list`, `checkpoint`, `snag`

**Files:** `internal/cli/start.go`, `internal/cli/stop.go`, `internal/cli/list.go`, `internal/cli/checkpoint.go`, `internal/cli/snag.go`
**Depends on:** 2.6

**Do:**
1. Each command is a thin wrapper that:
   - Opens the store (via config's DB path).
   - Creates a `session.Service`.
   - Calls the appropriate service method.
   - Prints the result as formatted text (or JSON with `--json` flag).
   - Closes the store.
2. Command definitions:
   - **`start`**: `taskflow start <task_id> <project_path>`. Prints session ID and agentic contract summary.
   - **`stop`**: `taskflow stop <session_id> [--summary "..."] [--status completed|paused|archived]`. Prints confirmation.
   - **`list`**: `taskflow list [--status active,paused] [--project /path]`. Prints table of sessions.
   - **`checkpoint`**: `taskflow checkpoint <session_id> --summary "..." [--files file1,file2]`. Prints checkpoint ID.
   - **`snag`**: `taskflow snag <session_id> --error "..." [--category build] [--resolution "..."]`. Prints snag ID + any matching resolutions.
3. All commands support `--json` flag for machine-readable output.

**Test:** Manual testing via the compiled binary:
- `taskflow start test-task-123 /tmp/testproject` → creates session.
- `taskflow list` → shows the session.
- `taskflow checkpoint <id> --summary "did stuff"` → creates checkpoint.
- `taskflow snag <id> --error "undefined: Foo"` → logs snag.
- `taskflow stop <id>` → completes session.
- `taskflow list --status completed` → shows completed session.

**Accept:** All five CLI commands work end-to-end against a local SQLite database. `--json` flag produces parseable JSON output.

---

### Phase 2 Checkpoint

**Verify all of the following before moving to Phase 3:**
- [ ] All domain types compile with proper validation
- [ ] Error signature normalization produces stable, matchable keys
- [ ] Git context capture works (and degrades gracefully)
- [ ] Store interface is fully implemented with all CRUD operations
- [ ] Session service orchestrates full lifecycle: start → checkpoint → snag → stop
- [ ] Agentic contract blob is correctly assembled with history
- [ ] Cross-project snag matching works in the store
- [ ] All 5 CLI commands work end-to-end
- [ ] All unit tests pass: `go test ./internal/...`

---

## Phase 3: MCP Server

> **Goal:** `taskflow serve` starts an MCP server that an LLM agent can invoke. All six MCP tools work end-to-end.

### Task 3.1: MCP Server Setup

**Files:** `internal/server/server.go`
**Depends on:** Phase 2

**Do:**
1. Create a `Server` struct that holds a `session.Service` and `store.Store`.
2. Implement `New(service *session.Service, store store.Store) *Server`.
3. Implement `Run(ctx context.Context) error`:
   - Create an `mcp-go` server instance.
   - Register all tool definitions (names, descriptions, input schemas).
   - Start the server on stdio transport.
4. Research `mcp-go` API: read `github.com/mark3labs/mcp-go` to understand tool registration, input schema definition, and response formatting. Adapt the server setup accordingly.

**Test:** `taskflow serve` starts without error and accepts MCP protocol handshake.
**Accept:** MCP server boots on stdio and registers all tools.

---

### Task 3.2: MCP Tool Handlers — Core Five

**Files:** `internal/server/tools.go`
**Depends on:** 3.1

**Do:**
1. Implement handler functions for each MCP tool. Each handler:
   - Extracts parameters from the MCP tool call request.
   - Calls the corresponding `session.Service` method.
   - Returns the result as JSON.
2. Tool definitions with input schemas:
   - **`tf_start`**: inputs `task_id` (string, required), `project_path` (string, required). Returns agentic contract JSON.
   - **`tf_checkpoint`**: inputs `session_id` (string, required), `summary` (string, required), `files` (array of strings, optional). Returns checkpoint JSON.
   - **`tf_snag`**: inputs `session_id` (string, required), `error` (string, required), `category` (string, optional), `resolution` (string, optional). Returns snag + matching resolutions JSON.
   - **`tf_list`**: inputs `status` (string, optional), `project_path` (string, optional). Returns session array JSON.
   - **`tf_stop`**: inputs `session_id` (string, required), `summary` (string, optional), `status` (string, optional). Returns confirmation JSON.
3. Error handling: catch service errors and return structured MCP error responses with human-readable messages. Never panic.

**Test:**
- Write Go tests that construct MCP tool call requests, invoke handlers directly, and verify JSON responses.
- Test error cases: invalid session_id, missing required params, stopping an already-stopped session.

**Accept:** All five core MCP tools return correct JSON responses and handle errors gracefully.

---

### Task 3.3: MCP Tool — `tf_init`

**Files:** `internal/server/tools.go` (add to existing)
**Depends on:** 3.2

**Do:**
1. Register `tf_init` tool with no required inputs.
2. Handler: For now (before Phase 4), return a placeholder response indicating ClickUp integration is not yet wired: `{"status": "ok", "message": "ClickUp workspace scan will be available after Phase 4"}`.
3. This establishes the tool in the MCP registry so agents can discover it.

**Test:** Call `tf_init` via MCP → get placeholder response.
**Accept:** `tf_init` tool is registered and callable.

---

### Task 3.4: Wire `serve` Command

**Files:** `internal/cli/serve.go` (update from placeholder)
**Depends on:** 3.1, 3.2, 3.3

**Do:**
1. Replace the placeholder implementation:
   - Load config, open store.
   - Create `session.Service`.
   - Create `server.Server`.
   - Call `server.Run(ctx)` — blocks until stdin closes or context is cancelled.
   - On shutdown: close store.
2. Handle `SIGINT`/`SIGTERM` for graceful shutdown.

**Test:** Start `taskflow serve`, send an MCP tool call for `tf_list` over stdin, verify JSON response on stdout.
**Accept:** `taskflow serve` runs as a functioning MCP server. Tools are callable over stdio.

---

### Phase 3 Checkpoint

**Verify all of the following before moving to Phase 4:**
- [ ] `taskflow serve` starts an MCP server on stdio
- [ ] All six MCP tools (`tf_start`, `tf_checkpoint`, `tf_snag`, `tf_list`, `tf_stop`, `tf_init`) are registered and callable
- [ ] Tool responses match the JSON contracts from the spec
- [ ] Agentic contract blob is returned by `tf_start` with correct history
- [ ] Error responses are structured and human-readable
- [ ] Graceful shutdown works on SIGINT
- [ ] All unit tests pass: `go test ./internal/...`

---

## Phase 4: ClickUp Integration

> **Goal:** Full bidirectional sync with ClickUp. Dynamic topology discovery. Status updates resolved by type, not name. Rate-limited async sync.

### Task 4.1: ClickUp API Client — Foundation

**Files:** `internal/clickup/client.go`, `internal/clickup/errors.go`
**Depends on:** Phase 3

**Do:**
1. Define `Client` struct:
   ```go
   type Client struct {
       httpClient *http.Client
       apiKey     string
       baseURL    string // "https://api.clickup.com/api/v2"
   }
   ```
2. Implement `New(apiKey string) *Client`.
3. Implement a private `do(ctx context.Context, method, path string, body, result interface{}) error`:
   - Builds the full URL.
   - Sets `Authorization: <apiKey>` header.
   - Sets `Content-Type: application/json`.
   - Marshals body to JSON (if non-nil).
   - Executes the request.
   - Checks status code: 200-299 → unmarshal response into `result`. 401/403 → return `ErrUnauthorized`. 429 → return `ErrRateLimited` with `Retry-After`. 5xx → return `ErrServerError`.
4. Define sentinel errors in `errors.go`: `ErrUnauthorized`, `ErrRateLimited`, `ErrNotFound`, `ErrServerError`.

**Test:**
- Unit test with `httptest.NewServer`: mock 200 response → verify result parsed. Mock 401 → verify `ErrUnauthorized`. Mock 429 with `Retry-After` → verify error contains backoff duration. Mock 500 → verify `ErrServerError`.

**Accept:** HTTP client handles all ClickUp response codes correctly with typed errors.

---

### Task 4.2: ClickUp API Client — Task Operations

**Files:** `internal/clickup/tasks.go`
**Depends on:** 4.1

**Do:**
1. Define response types matching ClickUp API v2:
   ```go
   type Task struct {
       ID          string   `json:"id"`
       Name        string   `json:"name"`
       Description string   `json:"text_content"`
       Status      TaskStatus `json:"status"`
       List        TaskList   `json:"list"`
   }
   type TaskStatus struct {
       Status string `json:"status"`
       Type   string `json:"type"`
   }
   type TaskList struct {
       ID   string `json:"id"`
       Name string `json:"name"`
   }
   ```
2. Implement `GetTask(ctx context.Context, taskID string) (*Task, error)`:
   - `GET /task/{taskID}`.
3. Implement `UpdateTaskStatus(ctx context.Context, taskID, statusName string) error`:
   - `PUT /task/{taskID}` with body `{"status": "<statusName>"}`.
4. Implement `PostComment(ctx context.Context, taskID, commentBody string) error`:
   - `POST /task/{taskID}/comment` with body `{"comment_text": "<commentBody>"}`.

**Test:**
- Unit test each method with `httptest` mocks. Verify correct HTTP method, path, headers, and body.

**Accept:** Task get, status update, and comment posting work against mocked ClickUp API.

---

### Task 4.3: ClickUp API Client — Workspace Discovery

**Files:** `internal/clickup/discovery.go`
**Depends on:** 4.1

**Do:**
1. Implement `GetWorkspaces(ctx context.Context) ([]Workspace, error)`:
   - `GET /team`.
2. Implement `GetSpaces(ctx context.Context, workspaceID string) ([]Space, error)`:
   - `GET /team/{workspaceID}/space`.
3. Implement `GetFolders(ctx context.Context, spaceID string) ([]Folder, error)`:
   - `GET /space/{spaceID}/folder`.
4. Implement `GetLists(ctx context.Context, folderID string) ([]List, error)`:
   - `GET /folder/{folderID}/list`.
5. Implement `GetFolderlessLists(ctx context.Context, spaceID string) ([]List, error)`:
   - `GET /space/{spaceID}/list` (lists not in any folder).
6. Implement `GetListDetails(ctx context.Context, listID string) (*ListDetail, error)`:
   - `GET /list/{listID}` — returns list info including its statuses array.
7. Define response types for each: `Workspace`, `Space`, `Folder`, `List`, `ListDetail`, `ListStatus` (with `Status`, `Type`, `OrderIndex` fields).

**Test:**
- Unit test each method with `httptest` mocks and sample JSON responses matching ClickUp API format.

**Accept:** Full workspace tree can be traversed via API: workspace → spaces → folders → lists → statuses.

---

### Task 4.4: ClickUp Cache — Store Methods & Population

**Files:** `internal/store/cache.go`
**Depends on:** 4.3

**Do:**
1. Add cache methods to the `Store` interface:
   ```go
   // ClickUp Cache
   UpsertCacheEntry(ctx context.Context, entry *CacheEntry) error
   GetCacheEntries(ctx context.Context, entityType, parentID string) ([]*CacheEntry, error)
   GetCacheEntry(ctx context.Context, entityType, entityID string) (*CacheEntry, error)
   GetListStatuses(ctx context.Context, listID string) ([]*CacheEntry, error)
   IsCacheStale(ctx context.Context, entityType, entityID string, ttl time.Duration) (bool, error)
   ClearCache(ctx context.Context) error
   ```
2. Implement all methods on `SQLiteStore`.
3. `IsCacheStale` returns true if the entry doesn't exist or if `fetched_at` is older than `ttl`.

**Test:**
- Unit test: upsert → get → verify. Upsert again with updated name → get → verify updated.
- Unit test: `IsCacheStale` with a 24-hour TTL returns false for fresh entries, true for old ones.
- Unit test: `GetListStatuses` returns only `status` entities for a given list parent_id.

**Accept:** Cache CRUD works. TTL checks are correct.

---

### Task 4.5: Workspace Scan Service

**Files:** `internal/clickup/scan.go`
**Depends on:** 4.3, 4.4

**Do:**
1. Implement `ScanWorkspace(ctx context.Context, client *Client, store store.Store) (*ScanResult, error)`:
   - Fetch workspaces.
   - For each workspace: fetch spaces.
   - For each space: fetch folders + folderless lists.
   - For each folder: fetch lists.
   - For each list: fetch list details (includes statuses).
   - Upsert all entities into `clickup_cache`.
   - Return a `ScanResult` summary (counts of workspaces, spaces, lists, statuses found).
2. Respect rate limiting: add a small delay between API calls (use the token bucket or simple `time.Sleep(600ms)` between requests).

**Test:**
- Unit test with `httptest` mocking the full workspace tree. Verify all entities are cached.

**Accept:** `ScanWorkspace` traverses the full ClickUp tree and populates `clickup_cache`.

---

### Task 4.6: Dynamic Status Resolution

**Files:** `internal/clickup/status.go`
**Depends on:** 4.4

**Do:**
1. Implement `ResolveStatus(ctx context.Context, store store.Store, client *Client, taskID string, targetType string) (string, error)`:
   - Get the task from ClickUp (or cache) to find its `list.id`.
   - Check if list statuses are cached and fresh.
   - If stale or missing, fetch list details and cache statuses.
   - Find the first status with `status_type` matching `targetType` (e.g., `active`, `closed`).
   - Return the status name string (what ClickUp expects in the update API).
   - If no match found, return empty string (caller logs warning and skips update).

**Test:**
- Unit test: cached statuses include `"In Progress" (type: active)` and `"Done" (type: closed)`. Resolve `active` → `"In Progress"`. Resolve `closed` → `"Done"`.
- Unit test: no `active`-type status exists → returns empty string.
- Unit test: stale cache triggers re-fetch.

**Accept:** Status resolution maps taskflow session states to ClickUp status names dynamically.

---

### Task 4.7: Sync Queue — Store Methods

**Files:** `internal/store/sync_queue.go`
**Depends on:** Phase 2 store

**Do:**
1. Add sync queue methods to the `Store` interface:
   ```go
   // Sync Queue
   EnqueueSync(ctx context.Context, op SyncOp) error
   DequeuePending(ctx context.Context, limit int) ([]*SyncOp, error)
   MarkInFlight(ctx context.Context, id int64) error
   MarkDone(ctx context.Context, id int64) error
   MarkFailed(ctx context.Context, id int64, errMsg string) error
   CountPendingByTaskID(ctx context.Context, taskID string) (int, error)
   ListRetryable(ctx context.Context) ([]*SyncOp, error)
   ```
2. Define `SyncOp` struct matching the `sync_queue` table.
3. `DequeuePending` returns items with `status = 'PENDING'` ordered by `created_at ASC`.
4. `MarkFailed` increments `retries`, sets `error_msg` and `last_attempted_at`. If `retries >= max_retries`, sets status to `FAILED` permanently.
5. `ListRetryable` returns items with `status = 'FAILED'` and `retries < max_retries`.

**Test:**
- Unit test full cycle: enqueue → dequeue → mark in flight → mark done.
- Unit test failure path: enqueue → dequeue → mark failed → verify retries incremented → list retryable returns it.
- Unit test max retries: mark failed 5 times → `ListRetryable` no longer returns it.

**Accept:** Sync queue operations work correctly with proper state transitions.

---

### Task 4.8: Sync Worker

**Files:** `internal/sync/worker.go`
**Depends on:** 4.1, 4.2, 4.6, 4.7

**Do:**
1. Define `Worker` struct:
   ```go
   type Worker struct {
       store   store.Store
       clickup *clickup.Client
       config  *config.SyncConfig
   }
   ```
2. Implement `Run(ctx context.Context) error`:
   - Loop: dequeue pending items (batch of 10).
   - For each item:
     - Mark in-flight.
     - Based on `operation`, call the appropriate ClickUp method:
       - `POST_COMMENT` → `client.PostComment(taskID, payload.Comment)`.
       - `UPDATE_STATUS` → resolve status name dynamically, then `client.UpdateTaskStatus(taskID, statusName)`.
       - `POST_SNAG` → `client.PostComment(taskID, payload.FormattedSnag)`.
     - On success: mark done.
     - On failure: mark failed with error message.
   - After processing a batch, sleep for `rate_limit_ms` between each API call (token bucket).
   - If no pending items, sleep 2 seconds before polling again.
   - On `ctx.Done()`: finish current in-flight item, then return.
3. Implement `DrainOnce(ctx context.Context) (int, error)`:
   - Process all pending + retryable items once (no loop). Return count processed.
   - Used by CLI commands and `taskflow sync`.

**Test:**
- Unit test with mock ClickUp client: enqueue 3 items → Run → verify all marked done, ClickUp methods called in order.
- Unit test with failing ClickUp client: verify items are marked failed with error messages.
- Unit test context cancellation: verify worker stops cleanly.
- Unit test `DrainOnce`: enqueue 2 items → drain → verify 2 processed.

**Accept:** Sync worker processes queue items in FIFO order, handles failures with retry, and shuts down gracefully.

---

### Task 4.9: Wire ClickUp into Session Service

**Files:** `internal/session/service.go` (update)
**Depends on:** 4.2, 4.6, 4.7, 4.8

**Do:**
1. Add `clickup.Client` and sync enqueue capability to `Service`:
   ```go
   type Service struct {
       store   store.Store
       config  *config.Config
       clickup *clickup.Client  // nil-safe: check before use
   }
   ```
2. Update `Start`:
   - If clickup client is set: validate task via API, cache task name/description, discover list statuses, enqueue `UPDATE_STATUS`.
   - If clickup client is nil: accept task_id as-is (for local-only testing during development).
3. Update `Checkpoint`: enqueue `POST_COMMENT`.
4. Update `LogSnag`: enqueue `POST_SNAG`.
5. Update `Stop`: enqueue `UPDATE_STATUS` + `POST_COMMENT`.
6. All enqueue calls happen inside the same database transaction as the local write.

**Test:**
- Unit test: with mock ClickUp client, Start enqueues an `UPDATE_STATUS` item.
- Unit test: Checkpoint enqueues a `POST_COMMENT` item with correct markdown.
- Unit test: Stop enqueues both `UPDATE_STATUS` and `POST_COMMENT`.

**Accept:** All session operations enqueue the appropriate ClickUp sync items.

---

### Task 4.10: Wire ClickUp into CLI Commands

**Files:** `internal/cli/init.go` (update), `internal/cli/serve.go` (update), `internal/cli/start.go` (update), all other CLI files
**Depends on:** 4.5, 4.8, 4.9

**Do:**
1. Update `taskflow init`:
   - After database init, if `CLICKUP_API_KEY` is set, run `ScanWorkspace` and print results.
   - With `--refresh`, clear existing cache and re-scan.
2. Update `taskflow serve`:
   - Create ClickUp client from config.
   - Pass client to `session.Service`.
   - Start sync worker as a background goroutine.
   - On shutdown: cancel worker context, wait for graceful drain (5s timeout).
3. Update all CLI commands:
   - Create ClickUp client from config.
   - Pass to session service.
   - After operation: if sync items were enqueued, run `worker.DrainOnce()` (best-effort, with a 10-second timeout).
4. Update `tf_init` MCP tool handler: call `ScanWorkspace` and return the scan result.

**Test:**
- Manual: `taskflow init` with a valid API key prints workspace topology.
- Manual: `taskflow start <real-task-id> /tmp/test` → session starts, ClickUp task status updates.
- Manual: `taskflow checkpoint → snag → stop` → verify comments appear on ClickUp task.

**Accept:** Full bidirectional ClickUp sync works. `taskflow init` discovers workspace topology. Status updates use dynamic resolution.

---

### Phase 4 Checkpoint

**Verify all of the following before moving to Phase 5:**
- [ ] ClickUp API client handles all response codes (200, 401, 429, 5xx)
- [ ] Workspace discovery traverses full tree and populates cache
- [ ] Status resolution maps by type, not by hardcoded name
- [ ] Sync queue processes items in FIFO order with retry/backoff
- [ ] Rate limiting enforces 600ms minimum between API calls
- [ ] `taskflow init` scans ClickUp workspace and caches topology
- [ ] `tf_start` validates task against ClickUp API and enqueues status update
- [ ] `tf_checkpoint` and `tf_snag` post comments to ClickUp
- [ ] `tf_stop` updates task status and posts session summary
- [ ] `tf_init` MCP tool returns workspace summary
- [ ] All unit tests pass: `go test ./internal/...`

---

## Phase 5: Resilience & Polish

> **Goal:** Production-ready for daily use. Integration tests, concurrent access hardening, documentation.

### Task 5.1: `sync` CLI Command

**Files:** `internal/cli/sync.go`
**Depends on:** 4.8

**Do:**
1. Register `sync` subcommand.
2. Behavior:
   - Open store and create ClickUp client.
   - Call `worker.DrainOnce()`.
   - Print progress: `Processed N items (M failed)`.
   - Include retryable failed items in the drain.

**Test:** Enqueue items via `taskflow start`, kill before sync completes. Run `taskflow sync` → verify items are processed.
**Accept:** `taskflow sync` drains all pending and retryable items.

---

### Task 5.2: Integration Tests

**Files:** `internal/integration_test.go`
**Depends on:** All of Phase 4

**Do:**
1. Use `//go:build integration` tag.
2. Test full workflow: start → checkpoint → snag → checkpoint → stop → start (resume).
3. Wire up: real SQLite (temp file) + `httptest` mock ClickUp + real git repo (temp dir).
4. Verify:
   - Agentic contract on resume contains last 3 checkpoints and unresolved snags.
   - Cross-project snag matching: create resolved snag in project A, start session in project B with matching error → `related_resolutions` is populated.
   - Sync queue items are created for each operation.
   - ClickUp mock received correct API calls in correct order.
5. Test concurrent access: two goroutines performing operations on different sessions simultaneously → no database errors.

**Test:** `go test -tags integration ./internal/...`
**Accept:** Full lifecycle and cross-project matching work end-to-end. Concurrent access doesn't cause errors.

---

### Task 5.3: Cache TTL & Refresh

**Files:** `internal/clickup/status.go` (update), `internal/clickup/scan.go` (update)
**Depends on:** 4.4, 4.5

**Do:**
1. In `ResolveStatus`: before using cached statuses, call `store.IsCacheStale` with 24-hour TTL. If stale, re-fetch list details and update cache.
2. In `ScanWorkspace`: update `fetched_at` on all upserted entries.
3. `taskflow init --refresh`: call `store.ClearCache()` before scanning.

**Test:**
- Unit test: set `fetched_at` to 25 hours ago → `IsCacheStale` returns true → re-fetch triggered.
- Unit test: fresh cache entry → no re-fetch.

**Accept:** Cache auto-refreshes after 24 hours. Manual refresh works via `--refresh`.

---

### Task 5.4: Error Message Polish

**Files:** All CLI and server files
**Depends on:** All prior tasks

**Do:**
1. Audit all error messages for clarity:
   - Wrap low-level errors with context: `fmt.Errorf("failed to start session: %w", err)`.
   - ClickUp auth errors: `"ClickUp API key is invalid or expired. Set CLICKUP_API_KEY and try again."`.
   - Missing task: `"ClickUp task '%s' not found. Verify the task ID is correct."`.
   - Active session conflict: `"An active session already exists in %s (session %s). Stop or pause it first."`.
2. Ensure MCP error responses include actionable messages.
3. CLI errors print to stderr with consistent formatting.

**Test:** Manually trigger each error case and verify the message is clear and actionable.
**Accept:** All error messages are human-readable and tell the user what to do.

---

### Task 5.5: README Documentation

**Files:** `README.md`
**Depends on:** All prior tasks

**Do:**
1. Write a comprehensive README covering:
   - What taskflow is (one paragraph).
   - Quick start: clone, set API key, build, init.
   - CLI usage with examples for each command.
   - MCP server setup: how to configure an agent to use `taskflow serve`.
   - Configuration reference (config.toml fields + env vars).
   - Architecture overview (link to design spec).
2. Keep it practical — focus on usage, not internals.

**Test:** Follow the README from scratch on a clean machine. Verify all steps work.
**Accept:** A new developer can go from zero to a working taskflow setup by following the README.

---

### Phase 5 Checkpoint (Final)

**Verify all of the following — this is the production-readiness gate:**
- [ ] Integration tests pass: `go test -tags integration ./internal/...`
- [ ] Concurrent access test passes (two simultaneous sessions)
- [ ] `taskflow sync` drains backed-up items
- [ ] Cache TTL auto-refreshes stale entries
- [ ] All error messages are clear and actionable
- [ ] README enables zero-to-working setup
- [ ] `go vet ./...` and `go build ./...` produce no warnings
- [ ] All unit tests pass: `go test ./internal/...`
- [ ] Portability verified: no hardcoded workspace IDs, space IDs, or status names anywhere in codebase
