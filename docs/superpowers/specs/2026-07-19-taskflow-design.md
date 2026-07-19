# Taskflow Design Specification

**Date:** 2026-07-19
**Status:** Draft
**Author:** Brandon Lamer-Connolly + Antigravity

---

## 1. Overview

Taskflow is a Go-based Model Context Protocol (MCP) server and CLI tool that synchronizes ClickUp task management with a local terminal-based coding workflow. It tracks "Coding Sessions" across multiple projects, acting as the source of truth for task status, progress checkpoints, and engineering snags (blockers). AI coding agents receive persistent context from taskflow — even across days, sessions, or projects — enabling self-healing behavior when past solutions are re-injected into the prompt.

### Core Thesis

An LLM agent loses context between invocations. Taskflow solves this by maintaining a structured session history that is returned to the agent every time a task is resumed. The cross-project snag index goes further: if the agent hit the same error in a different project last week and solved it, that resolution is surfaced automatically.

### Portability Principle

Taskflow must be plug-and-play for anyone who clones the repository. The setup contract is:

1. Clone the repo.
2. Set `CLICKUP_API_KEY`.
3. `go build && taskflow init`.

That's it. No hardcoded workspace IDs, space IDs, list IDs, or status names anywhere in the codebase. All ClickUp topology is discovered dynamically via the API key at runtime. Different developers have different ClickUp workspaces with different status labels, folder structures, and conventions — taskflow adapts to all of them because it queries ClickUp's API to understand the environment it's operating in.

Taskflow is also agent-agnostic. Any coding agent that speaks MCP can use it. The global SQLite database is the shared state, and the MCP server is the universal interface. No agent-specific configuration is required.

### Key Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Session concurrency | Fully parallel — multiple active sessions across projects | Developer context-switches between projects frequently |
| Database location | Single global SQLite at `~/.taskflow/sessions.db` | Enables cross-project snag matching and unified `tf_list` |
| ClickUp sync model | Local-first, async background sync | Tool calls return instantly; ClickUp outages never block work |
| Data model | Normalized tables (not JSONB blobs) | Makes snag index queryable and checkpoint history clean |
| Git context | Auto-capture with opt-out | Enriches checkpoints without manual effort |
| ClickUp integration depth | Full bidirectional sync (read context, post comments, update status) | Taskflow becomes the single pane of glass |
| ClickUp topology | Dynamically discovered via API, never hardcoded | Any developer's ClickUp workspace works out of the box |
| Status mapping | Resolved by ClickUp status type (`open`, `active`, `closed`), not name | Works regardless of what a workspace calls its statuses |
| Authentication | ClickUp API key via environment variable | Single setup step; rate-limit-aware client |
| Agent compatibility | Agent-agnostic via MCP standard | Any MCP-speaking agent works without configuration |

---

## 2. Technical Stack

| Component | Technology |
|---|---|
| Language | Go (latest stable) |
| Persistence | SQLite via `modernc.org/sqlite` (CGO-free, single binary) |
| Agent interface | `github.com/mark3labs/mcp-go` (MCP standard) |
| CLI framework | `github.com/spf13/cobra` |
| ClickUp API client | Standard `net/http` + `encoding/json` (ClickUp API v2) |
| UUID generation | `github.com/google/uuid` |
| Config file parsing | `github.com/BurntSushi/toml` |

---

## 3. Architecture & Package Layout

### Single Binary, Two Modes

`taskflow` compiles to one Go binary that operates in two modes:

- **`taskflow serve`** — Starts the MCP server via `mcp-go`, exposing tools for LLM agents over stdio.
- **`taskflow <command>`** — Runs CLI commands directly via `cobra` for human use in the terminal.

Both modes share the same core domain packages. CLI commands and MCP tool handlers are thin wrappers that call into the same session/store logic.

### Package Layout

```
taskflow/
├── cmd/taskflow/
│   └── main.go                # Entry point, wires cobra root command
├── internal/
│   ├── cli/                   # Cobra command definitions (start, stop, list, checkpoint, snag, serve)
│   ├── server/                # MCP server setup + tool handler registrations
│   ├── session/               # Core domain: Session, Checkpoint, Snag types + business logic
│   ├── store/                 # SQLite persistence (interface + implementation + migrations)
│   ├── clickup/               # ClickUp API client (tasks, comments, status updates)
│   ├── sync/                  # Async sync queue + background worker goroutine
│   ├── gitctx/                # Git context capture (branch, SHA, dirty files)
│   └── config/                # Config loading (env vars, TOML file, defaults)
├── docs/
├── go.mod
├── go.sum
└── README.md
```

### Package Boundaries

- **`session/`** owns the domain types and business rules. It knows nothing about SQLite, ClickUp, or MCP.
- **`store/`** depends on `session/` types but exposes a Go interface so implementations are swappable and testable.
- **`clickup/`** and **`sync/`** are purely infrastructure — they consume `session/` types but the domain doesn't know they exist.
- **`cli/`** and **`server/`** are the two entry points that wire everything together. They depend on `session/`, `store/`, `sync/`, and `config/`.

For each package, you should be able to answer: what does it do, how do you use it, and what does it depend on?

---

## 4. Data Model (SQLite Schema)

The database lives at `~/.taskflow/sessions.db`. WAL mode is enabled on open for concurrent read/write access.

### `sessions`

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `TEXT` | `PRIMARY KEY` | UUID, generated in Go |
| `task_id` | `TEXT` | `NOT NULL` | ClickUp task ID |
| `task_name` | `TEXT` | | Cached from ClickUp on start |
| `project_path` | `TEXT` | `NOT NULL` | Absolute filesystem path |
| `status` | `TEXT` | `NOT NULL` | `ACTIVE`, `PAUSED`, `COMPLETED`, `ARCHIVED` |
| `git_branch` | `TEXT` | | Captured on session start |
| `started_at` | `DATETIME` | `NOT NULL` | |
| `updated_at` | `DATETIME` | `NOT NULL` | |

### `checkpoints`

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `TEXT` | `PRIMARY KEY` | UUID |
| `session_id` | `TEXT` | `NOT NULL REFERENCES sessions(id)` | |
| `summary` | `TEXT` | `NOT NULL` | Markdown summary from agent |
| `files` | `TEXT` | | JSON array of file paths touched |
| `git_branch` | `TEXT` | | Branch at checkpoint time |
| `git_sha` | `TEXT` | | Commit SHA at checkpoint time |
| `git_dirty` | `TEXT` | | JSON array of uncommitted files |
| `created_at` | `DATETIME` | `NOT NULL` | |

### `snags`

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `TEXT` | `PRIMARY KEY` | UUID |
| `session_id` | `TEXT` | `NOT NULL REFERENCES sessions(id)` | |
| `error` | `TEXT` | `NOT NULL` | Full error text |
| `error_signature` | `TEXT` | `NOT NULL` | Normalized key for cross-project matching |
| `category` | `TEXT` | | `build`, `runtime`, `test`, `dependency` |
| `resolution` | `TEXT` | | How it was fixed (nullable until resolved) |
| `resolved` | `INTEGER` | `NOT NULL DEFAULT 0` | Boolean flag |
| `created_at` | `DATETIME` | `NOT NULL` | |
| `resolved_at` | `DATETIME` | | |

### `sync_queue`

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `INTEGER` | `PRIMARY KEY AUTOINCREMENT` | |
| `operation` | `TEXT` | `NOT NULL` | `POST_COMMENT`, `UPDATE_STATUS`, `POST_SNAG` |
| `task_id` | `TEXT` | `NOT NULL` | ClickUp task ID |
| `payload` | `TEXT` | `NOT NULL` | JSON blob with operation-specific data |
| `status` | `TEXT` | `NOT NULL DEFAULT 'PENDING'` | `PENDING`, `IN_FLIGHT`, `FAILED`, `DONE` |
| `retries` | `INTEGER` | `NOT NULL DEFAULT 0` | |
| `max_retries` | `INTEGER` | `NOT NULL DEFAULT 5` | |
| `created_at` | `DATETIME` | `NOT NULL` | |
| `last_attempted_at` | `DATETIME` | | |
| `error_msg` | `TEXT` | | Last failure reason |

### `clickup_cache`

Caches ClickUp topology discovered via the API so that status mapping and workspace navigation work without repeated API calls.

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `INTEGER` | `PRIMARY KEY AUTOINCREMENT` | |
| `entity_type` | `TEXT` | `NOT NULL` | `workspace`, `space`, `folder`, `list`, `status` |
| `entity_id` | `TEXT` | `NOT NULL` | ClickUp's ID for this entity |
| `parent_id` | `TEXT` | | Parent entity ID (e.g., a list's space ID) |
| `name` | `TEXT` | `NOT NULL` | Display name |
| `status_type` | `TEXT` | | For `status` entities: `open`, `active`, `closed`, `custom` |
| `order_index` | `INTEGER` | | Position within parent (for status ordering) |
| `extra` | `TEXT` | | JSON blob for additional metadata (color, etc.) |
| `fetched_at` | `DATETIME` | `NOT NULL` | When this was last fetched from ClickUp |

The cache uses a TTL model: entries older than 24 hours are considered stale and re-fetched on next access. Manual refresh is available via `taskflow init --refresh`.

### Indexes

```sql
CREATE INDEX idx_sessions_task_id ON sessions(task_id);
CREATE INDEX idx_sessions_status ON sessions(status);
CREATE INDEX idx_sessions_project_status ON sessions(project_path, status);
CREATE INDEX idx_snags_error_signature ON snags(error_signature);
CREATE INDEX idx_sync_queue_status ON sync_queue(status);
CREATE UNIQUE INDEX idx_clickup_cache_entity ON clickup_cache(entity_type, entity_id);
CREATE INDEX idx_clickup_cache_parent ON clickup_cache(entity_type, parent_id);
```

### Error Signature Normalization

When a snag is logged, the `error` text is normalized into `error_signature` by:

1. Stripping absolute file paths (replace `/Users/.../foo.go` with `<file>`).
2. Stripping line numbers (replace `:42:` with `:<line>:`).
3. Stripping timestamps and UUIDs.
4. Lowercasing.
5. Trimming whitespace.

This produces a stable signature so that `/Users/me/projectA/main.go:42: undefined: Foo` and `/Users/me/projectB/handler.go:99: undefined: Foo` both produce `<file>:<line>: undefined: foo`, enabling cross-project matching.

---

## 5. MCP Tools & CLI Commands

### Core Operations

These five operations are exposed as both MCP tools (via `mcp-go`) and CLI commands (via `cobra`). The underlying logic is identical.

#### `tf_start` — Start or resume a coding session

**Parameters:**

| Name | Type | Required |
|---|---|---|
| `task_id` | `string` | yes |
| `project_path` | `string` | yes |

**Behavior:**

1. Calls ClickUp API to validate `task_id` exists. Caches `task_name` and `task_description` locally.
2. **Discovers the task's list statuses** if not already cached (fetches list details, caches all available statuses with their types in `clickup_cache`).
3. Checks for an existing session with this `task_id`:
   - **No prior session** → creates a new `ACTIVE` session.
   - **`PAUSED` session exists** → resumes it (sets status to `ACTIVE`, updates `updated_at`).
   - **`ACTIVE` session already exists** → returns it idempotently (no duplicate created).
   - **`COMPLETED` or `ARCHIVED` session exists** → creates a new session (new attempt at the same task).
4. Captures git context (branch, SHA, dirty files) from `project_path`.
5. Writes `.b1codes/session.lock` in `project_path` containing the session ID.
6. Enqueues `UPDATE_STATUS` → ClickUp, resolved dynamically: finds the first status with `status_type = 'active'` from the cached list statuses. If no `active`-type status exists, logs a warning and skips the status update.
7. Returns the agentic contract blob.

**Agentic Contract (return value):**

```json
{
  "session_id": "uuid",
  "task": {
    "id": "abc123",
    "name": "Implement user auth",
    "description": "Full task description from ClickUp..."
  },
  "git": {
    "branch": "feature/auth",
    "sha": "a1b2c3d",
    "dirty_files": ["internal/auth/handler.go"]
  },
  "history": {
    "checkpoints": [
      {
        "summary": "Set up JWT middleware...",
        "files": ["internal/auth/jwt.go"],
        "created_at": "2026-07-18T14:30:00Z"
      }
    ],
    "snags": [
      {
        "error": "undefined: jwt.ParseWithClaims",
        "category": "build",
        "resolution": null,
        "created_at": "2026-07-18T15:00:00Z"
      }
    ],
    "related_resolutions": [
      {
        "error_signature": "undefined: jwt.parsewithclaims",
        "resolution": "Import was missing: added github.com/golang-jwt/jwt/v5",
        "source_project": "/Users/me/code/other-project",
        "resolved_at": "2026-07-10T09:00:00Z"
      }
    ]
  },
  "resumed": true
}
```

The `history.checkpoints` array contains the last 3 checkpoints (newest first). The `history.snags` array contains all unresolved snags for this task. The `history.related_resolutions` array is the cross-project superpower: resolved snags from any project whose `error_signature` matches an unresolved snag on this task.

#### `tf_checkpoint` — Record a progress checkpoint

**Parameters:**

| Name | Type | Required |
|---|---|---|
| `session_id` | `string` | yes |
| `summary` | `string` | yes |
| `files` | `[]string` | no |

**Behavior:**

1. Validates `session_id` exists and is `ACTIVE`.
2. Captures current git context from the session's `project_path`.
3. Inserts a row into `checkpoints`.
4. Updates `sessions.updated_at`.
5. Enqueues `POST_COMMENT` → ClickUp with formatted markdown:
   ```
   ## Progress Checkpoint: [timestamp]
   {summary}

   **Files:** {files}
   **Branch:** {branch} @ {sha}
   ```

**Returns:** The created checkpoint record.

#### `tf_snag` — Log an engineering snag

**Parameters:**

| Name | Type | Required |
|---|---|---|
| `session_id` | `string` | yes |
| `error` | `string` | yes |
| `category` | `string` | no |
| `resolution` | `string` | no |

**Behavior:**

1. Normalizes `error` into `error_signature` (strip paths, line numbers, timestamps).
2. Inserts into `snags`. If `resolution` is provided, sets `resolved = true` and `resolved_at`.
3. Queries for matching resolved snags across all projects by `error_signature`.
4. Enqueues `POST_SNAG` → ClickUp comment.

**Returns:** The snag record + any matching resolutions found (so the LLM gets immediate help).

#### `tf_list` — List sessions

**Parameters:**

| Name | Type | Required |
|---|---|---|
| `status` | `string` | no (defaults to `ACTIVE,PAUSED`) |
| `project_path` | `string` | no |

**Returns:** Array of session summaries with task name, status, last checkpoint timestamp, unresolved snag count, and pending sync count.

#### `tf_stop` — End a session

**Parameters:**

| Name | Type | Required |
|---|---|---|
| `session_id` | `string` | yes |
| `summary` | `string` | no |
| `status` | `string` | no (defaults to `COMPLETED`) |

**Behavior:**

1. If `summary` is provided, creates a final checkpoint.
2. Sets session status. Valid values: `COMPLETED`, `ARCHIVED`, `PAUSED`.
3. Removes `.b1codes/session.lock` from the project directory (unless `PAUSED`).
4. Enqueues `UPDATE_STATUS` → ClickUp, resolved dynamically from `clickup_cache`:
   - `COMPLETED` → first status with `status_type = 'closed'`
   - `PAUSED` → first status with `status_type = 'active'` (task is still in progress)
   - `ARCHIVED` → first status with `status_type = 'closed'`
   - If no matching status type is found, log a warning and skip the status update. Comments still post.
5. Enqueues `POST_COMMENT` → ClickUp with a session summary including duration, checkpoint count, snag count, and final resolution.

### CLI-Only Commands

- **`taskflow serve`** — Starts the MCP server on stdio. The sync worker runs as a background goroutine for the server's lifetime.
- **`taskflow init`** — Initializes `~/.taskflow/sessions.db` and **scans the user's ClickUp workspace topology** (workspaces → spaces → folders → lists → statuses), caching everything in `clickup_cache`. This is the one-time setup step after setting `CLICKUP_API_KEY`. Use `--refresh` to re-scan and update the cache. Also runs automatically (database creation only, not the full scan) on first use of any command.
- **`taskflow sync`** — Manually drains all `PENDING` and retryable `FAILED` items in the sync queue, then exits.
- **`taskflow config set <key> <value>`** — Set configuration values (e.g., `clickup_api_key`, `auto_git_context`).

### MCP-Only Tools

- **`tf_init`** — MCP-exposed version of `taskflow init`. Scans ClickUp workspace topology and returns a summary of discovered workspaces, spaces, and lists. Useful for an agent to understand the user's ClickUp environment.

---

## 6. Async Sync Engine

### Architecture

```
┌─────────────┐      INSERT       ┌─────────────┐      HTTP       ┌──────────┐
│  tf_start   │ ───────────────▶  │  sync_queue  │ ────────────▶  │ ClickUp  │
│  tf_checkpoint│   (immediate)   │  (SQLite)    │   (background) │  API v2  │
│  tf_snag    │                   │              │                │          │
│  tf_stop    │                   │  PENDING ──▶ │                │          │
└─────────────┘                   │  IN_FLIGHT   │ ◀── success ── │          │
                                  │  DONE / FAILED│               └──────────┘
                                  └─────────────┘
```

1. **Tool handlers** write to SQLite and enqueue a `sync_queue` row in the same database transaction. The tool call returns immediately to the caller.
2. **The sync worker** polls for `PENDING` items (FIFO via `ORDER BY created_at ASC`), marks them `IN_FLIGHT`, makes the ClickUp API call, then marks `DONE` or `FAILED`.
3. **On failure:** Increment `retries`, back off exponentially (1s → 2s → 4s → ... capped at 60s), try again. After `max_retries` (default 5), mark `FAILED` permanently and log a warning.
4. **Rate limiting:** A token bucket enforces a minimum 600ms interval between ClickUp API calls. ClickUp's personal API key limit is 100 requests/minute; 600ms spacing guarantees compliance even during burst syncs.

### Lifecycle by Mode

- **`taskflow serve`:** The sync worker starts as a goroutine, runs for the MCP server's lifetime, and performs a graceful drain on shutdown via `context.Context` cancellation with a 5-second timeout to finish in-flight items.
- **CLI commands:** If a CLI command enqueues sync work, it spins up a short-lived sync worker that drains just the items it enqueued, then exits. If ClickUp is unreachable, it logs a warning and exits — items stay `PENDING` for the next `serve` or `taskflow sync` to pick up.
- **`taskflow sync`:** Manual drain. Processes all `PENDING` and retryable `FAILED` items, printing progress to stdout, then exits.

### Ordering Guarantee

Items are processed in strict FIFO order. This ensures a "task completed" status update never arrives at ClickUp before the checkpoint comment that preceded it.

### Observability

`tf_list` includes a `pending_syncs` count per session so backed-up sync items are visible.

---

## 7. Configuration

All configuration lives in `~/.taskflow/config.toml`, with environment variable overrides taking precedence.

```toml
[clickup]
api_key = ""              # Override: CLICKUP_API_KEY env var

[git]
auto_context = true       # Capture git state on start/checkpoint

[sync]
max_retries = 5           # Max retry attempts per sync item
rate_limit_ms = 600       # Minimum ms between ClickUp API calls
drain_timeout_s = 5       # Graceful shutdown drain window

[database]
path = "~/.taskflow/sessions.db"
```

**Precedence:** Environment variable > config file > compiled default.

The `~/.taskflow/` directory and `config.toml` are created automatically on first use with default values.

---

## 8. Error Handling

### Strategy by Layer

| Layer | Error Type | Handling |
|---|---|---|
| **Store (SQLite)** | DB locked, corrupt, migration failure | Return wrapped Go errors with context. Callers decide retry vs fail. |
| **ClickUp client** | 401/403 (auth) | Fail fast, do not retry. Log: "ClickUp API key invalid or expired." |
| **ClickUp client** | 429 (rate limit) | Respect `Retry-After` header, back off, retry. |
| **ClickUp client** | 5xx / network error | Exponential backoff, retry up to `max_retries`. |
| **Git context** | Not a git repo, git not installed | Non-fatal. Set git fields to `null`, proceed normally. |
| **Session lock** | Cannot write `.b1codes/session.lock` (permissions) | Warn but do not block session creation. Lock file is advisory. |
| **MCP tool handler** | Any error | Return structured MCP error response with human-readable message. Never panic. |
| **CLI command** | Any error | Print to stderr, exit code 1. |

### Core Principle

ClickUp and git are enrichment layers. If they fail, the core session tracking (SQLite) still works. Taskflow never refuses to start a session because ClickUp is unreachable.

---

## 9. Concurrency

### SQLite WAL Mode

SQLite with Write-Ahead Logging handles concurrent access (e.g., two terminals, or an MCP server + CLI command running simultaneously):

1. **Enable WAL** on database open: `PRAGMA journal_mode=WAL;` — allows concurrent readers alongside one writer.
2. **Busy timeout:** `PRAGMA busy_timeout=5000;` — wait up to 5 seconds for a write lock rather than failing immediately.
3. **Short transactions:** Keep write transactions minimal (insert one row, update one status). No long-held locks.

### Advisory Session Lock

The `.b1codes/session.lock` file in each `project_path` acts as a soft advisory lock. Before starting a session, `tf_start` checks if a lock file exists and whether the session it references is still `ACTIVE`. If so, it returns an error: "An active session already exists in this project." This prevents two agents from unknowingly working the same project simultaneously.

---

## 10. Testing Strategy

### Unit Tests

Each package has `*_test.go` files alongside the source:

- **`session/`** — Domain logic: error signature normalization, status transition validation, checkpoint and snag business rules.
- **`store/`** — In-memory SQLite (`:memory:`) for fast, isolated tests. Cover every query: insert, list, filter, cross-project snag lookup, sync queue drain.
- **`clickup/`** — `net/http/httptest` to mock ClickUp API responses (200, 401, 429, 5xx). Verify request formatting, auth headers, rate-limit backoff.
- **`gitctx/`** — Temp directory initialized with `git init` in `TestMain`. Verify branch/SHA/dirty-file capture and graceful fallback when not in a git repo.
- **`sync/`** — Mock ClickUp client. Verify FIFO ordering, retry/backoff logic, max-retry exhaustion, graceful context-cancelled shutdown.

### Integration Tests

Located in `internal/integration_test.go` or behind a `//go:build integration` tag:

- Wire up real SQLite + mock ClickUp + real git repo in a temp directory.
- Run full workflows: `start → checkpoint → snag → checkpoint → stop`.
- Verify the agentic contract blob returned by `tf_start` on resume contains the correct history (last 3 checkpoints, unresolved snags, cross-project resolutions).
- Verify cross-project snag matching end-to-end: create a snag with a resolution in project A, start a session in project B with a matching error, confirm the resolution surfaces in `related_resolutions`.

### No External-Service Tests in CI

ClickUp integration is validated manually or behind a `//go:build clickup` tag that only runs when `CLICKUP_API_KEY` is set.

---

## 11. Development Phases

| Phase | Scope | Deliverable |
|---|---|---|
| **Phase 1: Skeleton** | `cmd/taskflow/main.go`, cobra root + `serve`/`init` commands, SQLite initialization with all table migrations (including `clickup_cache`), `config/` package, WAL mode setup | A binary that boots, creates `~/.taskflow/`, initializes the database, and responds to `taskflow init` and `taskflow --help` |
| **Phase 2: Core Sessions** | `session/`, `store/`, `gitctx/`, all CLI commands (start/stop/list/checkpoint/snag), `.b1codes/session.lock` management | Fully functional local session tracking via CLI. No MCP, no ClickUp. All unit tests passing. |
| **Phase 3: MCP Server** | `server/` package, `mcp-go` integration, register all five core MCP tools + `tf_init`, wire handlers to same `session/` logic as CLI | `taskflow serve` starts an MCP server that an LLM agent can invoke. The agentic contract blob works end-to-end. |
| **Phase 4: ClickUp Integration** | `clickup/` client with dynamic topology discovery, `clickup_cache` population, `sync/` queue + worker, enqueue logic wired into tool handlers, dynamic status resolution | Full bidirectional sync. `taskflow init` scans ClickUp workspace. Checkpoints and snags appear as ClickUp comments. Status updates resolved by type, not name. Rate limiting works. |
| **Phase 5: Resilience & Polish** | Integration tests, concurrent access hardening, `taskflow sync` CLI command, cache TTL/refresh, error message polish, README documentation | Production-ready for daily use. Plug-and-play: clone, set API key, build, init, go. |
