# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Taskflow is a Go CLI + MCP server that connects local coding sessions to ClickUp tasks: it tracks session start/checkpoint/stop lifecycle, captures git context, and asynchronously syncs progress comments and status updates back to ClickUp. It exposes the same functionality two ways — as Cobra CLI commands (`./taskflow start ...`) and as MCP tools (`tf_start`, `tf_checkpoint`, etc.) served over stdio for LLM agent integration.

## Commands

```bash
go build ./cmd/taskflow       # build the binary (outputs ./taskflow)
go test ./...                 # run all tests
go test ./internal/session/... -run TestName   # run a single test
go vet ./...
```

There is no Makefile or CI config in this repo — the above `go` commands are the only build/test tooling.

Running the built binary:
```bash
./taskflow init                # scan ClickUp workspace, populate local cache
./taskflow start <task_id> <project_path>
./taskflow checkpoint <session_id> --summary "summary" --files a.go,b.go
./taskflow decision <session_id> --topic "..." --decision "..." --rationale "..."
./taskflow snag <session_id> --error "error text" --category build
./taskflow stop <session_id> --summary "..." --status completed
./taskflow list --status active,paused
./taskflow serve                # run as MCP server over stdio
./taskflow sync                 # manually drain the sync queue once
```

Config lives at `~/.taskflow/config.toml` (auto-created on first run with defaults from `internal/config/config.go`'s `DefaultConfigContent`). `CLICKUP_API_KEY` env var overrides the config file's key. The SQLite DB path defaults to `~/.taskflow/sessions.db`.

## Architecture

Layered dependency flow: `cli` and `server` are two thin front-ends over the same `session.Service`, which is the sole orchestrator of business logic. Neither front-end talks to `store` or `clickup` directly except to construct and inject dependencies.

- **`internal/session`** — the core. `Service` (service.go) implements Start/Checkpoint/LogSnag/List/Stop against the `SessionStore` interface (defined here, implemented by `store`). It depends on two small interfaces it also defines — `ClickUpClient` and `GitCapturer` — implemented by shared adapters in `internal/adapters` wrapping `internal/clickup` and `internal/gitctx`. This inversion means `session` has zero import of `clickup` or `gitctx` packages directly; check `service.go`'s interfaces before adding new external calls.
- **`internal/adapters`** — shared implementations of `session.GitCapturer` (`GitCapturer` wrapping `internal/gitctx`) and `session.ClickUpClient` (`ClickUpAdapter` wrapping `internal/clickup`). Used by both `cli` (via `newSessionService`) and `server` to construct `session.Service`.
- **`internal/store`** — SQLite (via `modernc.org/sqlite`, no CGo) persistence. `Store` interface embeds `session.SessionStore` plus ClickUp cache and sync-queue methods. `migrations.go` holds a versioned, append-only list of SQL migrations run against a `schema_version` table on every `store.New()` — add new schema changes as a new entry in the `migrations` slice, never edit an existing one.
- **`internal/clickup`** — HTTP client for the ClickUp API (`client.go`), hierarchy discovery (`discovery.go`: workspace → space → folder → list → statuses), and `ScanWorkspace` (`scan.go`) which walks that hierarchy and caches it via `store`. `status.go`'s `ResolveStatusForRole` maps a lifecycle role (`start`/`reopen`/`complete`/`archive`, sent by `session.Service` as the `UPDATE_STATUS` payload `{"role": …}`; `Start` sends `reopen` when the task's live ClickUp status is `done`/`closed`) to a status on the task's list; `ApplyStatusRole` resolves and applies it. The role constants live in `session` (`session.StatusRole`) and `clickup.StatusRole` is a type alias, so both sides share them. ClickUp status types are only `open`/`custom`/`done`/`closed` — never `active` — so roles resolve by type + orderindex + whole-word name matching, with optional `[clickup.status_map]` overrides; `archive` only targets a cancel-like closed status, never a plain "complete". It fetches the task live and returns `ErrSkipStatusChange` when the role doesn't apply to the task's current status (`start` only from `open`; `reopen` from `open`/`done`/`closed`; `complete`/`archive` never from `done`/`closed`), so taskflow never undoes a status someone else set. Statuses are cached per list (key `<list_id>/<name>`, replaced atomically via `store.ReplaceListStatuses`) with a 24h staleness check; the cache is also refetched when the task's live status isn't in it, when an override misses (a confirmed miss is trusted for 10 minutes), or when ClickUp rejects a cached name with a 400 (then the update is retried once if the refreshed list resolves to a different name; a final rejection wraps `ErrStatusRejected`).
- **`internal/sync`** — `Worker` drains `sync_queue` rows written by `session.Service.enqueueSync`. Two modes: `Run` (ticker-driven background loop, started as a goroutine by `cli/serve.go` alongside the MCP server so checkpoints/stops are automatically synced to ClickUp) and `DrainOnce` (single pass, used by `taskflow sync`). Failed ops are retried up to `max_retries`, except errors that can't succeed on retry (`isPermanent`: unknown role/operation, no matching status, a 400 to the op's own write — the comment POST or the final status update — but not to a lookup along the way), which go straight to `MarkFailedPermanent`. `SyncConfig.RateLimitMS` is enforced per HTTP request by `clickup.Client.WithMinInterval`; a 429's `Retry-After` holds back every request on the client. `serve` shares one paced client between the MCP tools and the worker.
- **`internal/gitctx`** — captures branch/SHA/dirty-files for a project path; used by `internal/adapters.GitCapturer`.
- **`internal/cli`** — one file per Cobra subcommand, registered via `init()` on the package-level `rootCmd` in `root.go`. `PersistentPreRunE` on root loads config into the command context (`ConfigFromContext`); every subcommand opens its own `store.New(cfg.DBPath())` and closes it via `defer`.
- **`internal/server`** — MCP server (`mark3labs/mcp-go`). `registerTools()` maps each `tf_*` tool to a handler that parses MCP args and calls into the same `session.Service` methods the CLI uses with shared adapters from `internal/adapters`.

### Session state machine

Status values: `ACTIVE`, `PAUSED`, `COMPLETED`, `ARCHIVED` (`internal/session/status.go`). Transitions are validated by `ValidateTransition`: `ACTIVE` and `PAUSED` can go anywhere; `COMPLETED` can only move to `COMPLETED` or `ARCHIVED`; `ARCHIVED` is terminal. `Service.Start` treats an existing `PAUSED` session for the same task as a resume (reuses session ID); `COMPLETED`/`ARCHIVED` sessions cause a new session to be created instead.

### Locking

A per-project advisory lock lives at `<project_path>/.b1codes/session.lock` (`internal/session/lock.go`), containing the active session ID. `Service.Start` refuses to start a *different* task in a project that already has an active session locked, unless the existing session isn't actually `ACTIVE` anymore.

### Agentic contract

`Service.BuildAgenticContract` assembles the JSON payload returned by `tf_start`/CLI `start` — recent checkpoints, logged decisions & dialogue rationales, unresolved snags, and cross-session "related resolutions" (snags with a matching normalized error signature resolved in *other* sessions/projects, found via `store.FindMatchingResolutions`). This is the primary mechanism for giving an LLM agent continuity across session starts/resumes.

## Design docs

`docs/superpowers/specs/` and `docs/superpowers/plans/` contain the original design spec and implementation plan this codebase was built from — useful background if extending the architecture significantly.
