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

# Projects with a completeness manifest (see "Completeness" below)
[completeness]
gate = "review"          # report | review | block
allow_commands = false   # run the manifest's command checks (test suites)
command_timeout_s = 300

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
| `verify` | Run the session's command checks now (needs `allow_commands`) | `taskflow verify <session_id>` |
| `check` | Exit 2 while required obligations are open; `--hook claude` for a Claude Code Stop hook | `taskflow check --project . [--hook claude]` |
| `completeness report` | Drift report: where rules aren't followed, per b1 module | `taskflow completeness report --since 30d --format b1` |
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
| `tf_verify` | `session_id` (string) | Runs the session's command checks now (test suites, Storybook test-runner); needs `allow_commands` |
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

`tf_checkpoint` responses gain a `completeness` section (`newly_open`, `newly_satisfied`, `still_open`), ClickUp checkpoint comments gain a `**Completeness:** 2/5 · open: story, e2e` line, and the "Session Finished" comment lists every obligation. Completing a session with required work open goes through the completion gate (`[completeness] gate`): `review` moves the ClickUp task to review, `block` refuses until the work is done, attested, or waived. Without a manifest, nothing changes. See `docs/superpowers/specs/2026-10-10-completeness-design.md` for the full schema (stacks, command resolution, every `verify` check).

A `deleted:` trigger fires on a file removed since the session started (or a rename's old path), and the `absent:` check passes once nothing matches, e.g. "a deleted screen's stories go too":

```yaml
  - id: react-screen-deleted
    when: { deleted: "src/features/{feature}/pages/{name}.tsx" }
    require:
      - id: stories-removed
        describe: "Remove the stories for {name}"
        verify: { absent: "src/features/{feature}/pages/{name}.stories.tsx" }
```

### Command checks

A requirement's `run:` (a stack command such as `{ command: test }`, or a literal command line from the project overrides) is a command check. With `allow_commands = true` in `~/.taskflow/config.toml`, taskflow runs it with `sh -c` in the stack's `root`, with the stack's `env` over the inherited environment and a `command_timeout_s` limit (the whole process group is killed on timeout). Exit status 0 passes and adds `command` to the obligation's verified levels; anything else leaves it open, with the end of the output as evidence. Commands run when a session is completed and on demand with `tf_verify` / `taskflow verify`, never on checkpoints, and they come only from the manifest and overrides, never from tool arguments. A pass counts only while the project is as it was when the command ran: each run is tied to a fingerprint of the content under its stack root (the git tree it would commit as, untracked files included, ignored ones not) and the stack env. After any change, the obligation is open again ("stale") until the command runs again, so `taskflow check` and the hooks never pass on an old result. Outside git there is no fingerprint, and a pass counts only for the run that made it. With commands off, or no command resolved for the stack, `attest_when: no_command` falls back to an attestation.

### Hooks

The completion gate only fires when the agent calls `tf_stop`. `taskflow check` runs outside the agent, so it can't be skipped. It finds the session from `.b1codes/session.lock`, evaluates it, and exits 2 while required obligations are open. As a Claude Code `Stop` hook (`b1 pair` installs it), it answers with Claude Code's block response listing the open work, so the agent keeps going. It allows the stop once the hook input has `stop_hook_active`, so it can't loop:

```json
{
  "hooks": {
    "Stop": [{ "hooks": [{ "type": "command",
      "command": "taskflow check --project \"$CLAUDE_PROJECT_DIR\" --hook claude" }] }]
  }
}
```

For any agent, an optional git `pre-push` hook keeps unfinished work off the remote (`.git/hooks/pre-push`, executable):

```sh
#!/bin/sh
exec taskflow check --project "$(git rev-parse --show-toplevel)"
```

### Drift report

`taskflow completeness report [--project PATH] [--since 30d] [--format text|json|b1]` aggregates obligations per `(source, rule, requirement)`: satisfied before any reminder (`AGENTS.md` worked), satisfied only after reminders (the guidance is weak), attested, waived (similar rationales clustered), and open at stop (the rule isn't followed). `--format b1` turns the patterns into suggestions aimed at the b1 module files to change (`modules/<module>/context/`, `modules/<module>/completeness.yaml`). Nothing is changed automatically.

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
- **`internal/completeness`**: Loads and validates the completeness manifest, resolves per-stack test commands, and checks a changed-file set against its rules (it executes nothing; command checks are run by `internal/adapters`).
- **`internal/drift`**: Builds the completeness drift report from the store's per-requirement counts: waiver-rationale clusters, signals, and b1 module suggestions.
- **`internal/server`**: JSON-RPC Model Context Protocol (MCP) server implementation wrapping `session.Service`.

---

## Testing

Run all unit and integration tests:

```bash
go test ./...
```