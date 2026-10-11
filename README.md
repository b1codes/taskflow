# Taskflow

Taskflow is a developer productivity engine and context layer that connects your local coding workflows with ClickUp tasks. It operates as both a command-line interface (CLI) and a Model Context Protocol (MCP) server for seamless LLM pair-programming integration (e.g. Google Antigravity, Claude Code).

---

## Key Features

- **Session Lifecycle Management**: Start, checkpoint, pause, resume, and stop task-aligned coding sessions.
- **Agentic Session Contract**: Returns structured session history, checkpoints, dialogue rationales, and past snags/resolutions to maintain context across restarts.
- **Git Context Capturing**: Automatically tracks dirty files, active branch names, and commit diffs.
- **Dialogue & Rationale Tracking**: Log design choices, architectural trade-offs, and non-action justifications directly to ClickUp.
- **Cross-Session Snag Resolution**: Matches recurring error signatures against past resolutions across projects.
- **Resilient Async Sync**: SQLite WAL-mode queue with rate-limited background worker and retry logic to reliably sync progress comments and statuses to ClickUp.
- **Advisory Project Locking**: Prevents conflicting sessions on the same local workspace via `.b1codes/session.lock`.

---

## Installation & Setup

### 1. Build & Install

```bash
# Build binary locally
go build -o taskflow ./cmd/taskflow

# Or install directly to $GOPATH/bin
go install ./cmd/taskflow
```

### 2. Configuration

Taskflow stores its configuration at `~/.taskflow/config.toml` (auto-created with `0600` permissions on first run):

```toml
[clickup]
api_key = "pk_your_clickup_api_key"

# Optional — see "Status mapping" below
[clickup.status_map]
# start = "in progress"
# complete = "complete"
# archive = "cancelled"

[git]
auto_context = true

[sync]
max_retries = 5
rate_limit_ms = 600
drain_timeout_s = 5

[database]
path = "~/.taskflow/sessions.db"
```

> [!TIP]
> You can also supply your ClickUp API key via environment variable:
> ```bash
> export CLICKUP_API_KEY="pk_your_clickup_api_key"
> ```

#### Status mapping

Taskflow moves your ClickUp task through lifecycle *roles*, not hardcoded status names, so it works with any list's statuses:

| Role | When | Default status on the task's list |
|---|---|---|
| `start` | Session started or resumed | The `custom` status named like "in progress" (preferred), "doing", or "wip", matched by whole word; else the first `custom` status that isn't pre-work ("ready", "backlog", "to do") or a review, QA, or blocked stage |
| `reopen` | New session on a task whose last session was completed or archived | Same as `start` |
| `complete` | Session stopped as completed | First `done` status (e.g. "complete"), else first `closed` |
| `archive` | Active/paused session archived (abandoned) | First `closed` status named like a cancellation ("cancelled", "won't do", "duplicate"); otherwise unchanged, so abandoned work is never shown as complete |

Taskflow checks the task's live status first and never moves it back from a status someone else chose: `start` only moves a task that is still `open`, so a task that's in review or done stays put, and `complete`/`archive` leave tasks that are already `done` or `closed`. `reopen` may also move a `done`/`closed` task back to work, since you're starting a new session on work you'd finished. Pausing and archiving an already-completed session leave the task's status alone. To pin a role to a specific status, set it under `[clickup.status_map]` (matched case-insensitively; `reopen` uses `start`'s entry; a name missing from the list is re-checked against ClickUp once, then falls back to the default).

### 3. Initialize Workspace & Cache

Scan your ClickUp workspace hierarchy (spaces, folders, lists, custom statuses) and populate local cache:

```bash
taskflow init
```

---

## CLI Usage

| Command | Description | Example |
| :--- | :--- | :--- |
| `init` | Scan & cache ClickUp workspace | `taskflow init` |
| `start` | Start or resume a session | `taskflow start <task_id> <project_path>` |
| `checkpoint` | Record progress & touched files | `taskflow checkpoint <session_id> --summary "Refactored user auth" --files auth.go,router.go` |
| `decision` | Log design choices & rationale | `taskflow decision <session_id> --topic "Auth" --decision "Use JWT" --rationale "Stateless scalability" --status accepted` |
| `snag` | Record blocker / error signature | `taskflow snag <session_id> --error "go build failed" --category build --resolution "Fixed import"` |
| `list` | List active / past sessions | `taskflow list --status active,paused` |
| `stop` | Stop or pause a session | `taskflow stop <session_id> --summary "Completed auth module" --status completed` |
| `completeness` | Report what the session still owes under the project's completeness manifest | `taskflow completeness <session_id>` |
| `sync` | Manually drain async sync queue | `taskflow sync` |
| `serve` | Run stdio MCP server + sync worker | `taskflow serve` |

### Global Flags
- `--json`: Output result in structured JSON format.
- `-v, --verbose`: Enable verbose debug logging.

---

## MCP Server Integration

Taskflow exposes its capabilities over stdio via the Model Context Protocol (MCP).

### Antigravity Integration

Add Taskflow to your global Antigravity MCP configuration (`~/.gemini/config/mcp_config.json`):

```json
{
  "mcpServers": {
    "taskflow": {
      "command": "/Users/brandonlamer-connolly/devkits/go/bin/taskflow",
      "args": ["serve"],
      "env": {
        "CLICKUP_API_KEY": "pk_your_clickup_api_key"
      }
    }
  }
}
```

### Claude Desktop / Claude Code Integration

Add Taskflow to your Claude MCP config (e.g. `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "taskflow": {
      "command": "taskflow",
      "args": ["serve"]
    }
  }
}
```

### Available MCP Tools

| Tool | Parameters | Purpose |
| :--- | :--- | :--- |
| `tf_start` | `task_id` (string), `project_path` (string) | Starts/resumes a coding session and returns the Agentic Contract |
| `tf_checkpoint` | `session_id` (string), `summary` (string), `files` (string[]) | Records a progress checkpoint and modified files |
| `tf_decision` | `session_id` (string), `topic` (string), `decision` (string), `rationale` (string), `status` (string, opt), `files` (string[], opt) | Logs design decisions and dialogue rationales |
| `tf_snag` | `session_id` (string), `error` (string), `category` (string, opt), `resolution` (string, opt) | Logs an engineering obstacle or resolved bug |
| `tf_list` | `status` (string, opt), `project_path` (string, opt) | Queries coding sessions |
| `tf_stop` | `session_id` (string), `summary` (string, opt), `status` (string, opt) | Completes (`completed`), pauses (`paused`), or archives (`archived`) a session |
| `tf_completeness` | `session_id` (string) | Re-checks the session's changes against the completeness manifest and reports open obligations |
| `tf_init` | `refresh` (bool, opt) | Scans ClickUp hierarchy and updates local cache |

---

## Completeness (Definition of Done)

A project can declare what "done" means in `.agents/completeness.yaml` (generated by `b1 pair`) or a hand-written `.taskflow/completeness.yaml`. Taskflow re-reads it on every start, checkpoint, and stop, compares it against everything changed since the session started (committed, uncommitted, and untracked), and records **obligations** — work still owed:

```yaml
version: 1
rules:
  - id: react-screen
    describe: "New screen"
    when:
      added: "src/features/{feature}/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx", "**/*.test.tsx"]
    require:
      - id: story
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.stories.tsx"
          contains: ["export default", "{name}"]
```

`tf_checkpoint` responses gain a `completeness` section (`newly_open`, `newly_satisfied`, `still_open`), ClickUp checkpoint comments gain a `**Completeness:** 2/5 · open: story, e2e` line, and the "Session Finished" comment lists every obligation. Nothing blocks yet. Without a manifest, nothing changes. See `docs/superpowers/specs/2026-10-10-completeness-design.md` for the full schema (stacks, command resolution, every `verify` check).

---

## Architecture

Taskflow employs a clean, decoupled architecture:

```
                  ┌────────────────┐      ┌────────────────┐
                  │   Cobra CLI    │      │   MCP Server   │
                  └───────┬────────┘      └────────┬───────┘
                          │                        │
                          ▼                        ▼
                  ┌────────────────────────────────────────┐
                  │             session.Service            │
                  │   (State Machine, Locking, Contracts)  │
                  └─────────┬───────────────────┬──────────┘
                            │                   │
                ┌───────────▼────────┐  ┌───────▼───────────┐
                │  adapters.GitCtx   │  │ adapters.ClickUp  │
                └────────────────────┘  └───────┬───────────┘
                                                │
                                        ┌───────▼───────────┐
                                        │  sync.Worker      │
                                        │  (Async Queue)    │
                                        └───────┬───────────┘
                                                │
                                                ▼
                                        ┌───────────────┐
                                        │ SQLite Store  │
                                        │ (WAL Mode)    │
                                        └───────────────┘
```

- **`internal/session`**: Core orchestration layer managing session state transitions (`ACTIVE`, `PAUSED`, `COMPLETED`, `ARCHIVED`), advisory project locks (`.b1codes/session.lock`), and agentic contract assembly.
- **`internal/store`**: Pure-Go SQLite persistence layer (`modernc.org/sqlite`) with versioned, append-only migrations.
- **`internal/clickup`**: ClickUp REST client, workspace topology discovery, and custom status resolution with 24h caching.
- **`internal/sync`**: Rate-limited background worker consuming `sync_queue` tasks with exponential backoff retries.
- **`internal/gitctx`**: Fast Git inspection layer retrieving branch, SHA, status, dirty diffs, and the files changed since a session's base commit.
- **`internal/completeness`**: Loads and validates the completeness manifest, resolves per-stack test commands, and checks a changed-file set against its rules.
- **`internal/server`**: JSON-RPC Model Context Protocol (MCP) server implementation wrapping `session.Service`.

---

## Testing

Run all unit and integration tests:

```bash
go test ./...
```