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
./taskflow checkpoint <session_id> "summary" --files a.go,b.go
./taskflow snag <session_id> "error text" --category build
./taskflow stop <session_id> --summary "..." --status completed
./taskflow list --status active,paused
./taskflow serve                # run as MCP server over stdio
./taskflow sync                 # manually drain the sync queue once
```

Config lives at `~/.taskflow/config.toml` (auto-created on first run with defaults from `internal/config/config.go`'s `DefaultConfigContent`). `CLICKUP_API_KEY` env var overrides the config file's key. The SQLite DB path defaults to `~/.taskflow/sessions.db`.

## Architecture

Layered dependency flow: `cli` and `server` are two thin front-ends over the same `session.Service`, which is the sole orchestrator of business logic. Neither front-end talks to `store` or `clickup` directly except to construct and inject dependencies.

- **`internal/session`** — the core. `Service` (service.go) implements Start/Checkpoint/LogSnag/List/Stop against the `SessionStore` interface (defined here, implemented by `store`). It depends on two small interfaces it also defines — `ClickUpClient` and `GitCapturer` — so both `cli` and `server` provide their own adapter implementations wrapping `internal/clickup` and `internal/gitctx`. This inversion means `session` has zero import of `clickup` or `gitctx` packages directly; check `service.go`'s interfaces before adding new external calls.
- **`internal/store`** — SQLite (via `modernc.org/sqlite`, no CGo) persistence. `Store` interface embeds `session.SessionStore` plus ClickUp cache and sync-queue methods. `migrations.go` holds a versioned, append-only list of SQL migrations run against a `schema_version` table on every `store.New()` — add new schema changes as a new entry in the `migrations` slice, never edit an existing one.
- **`internal/clickup`** — HTTP client for the ClickUp API (`client.go`), hierarchy discovery (`discovery.go`: workspace → space → folder → list → statuses), and `ScanWorkspace` (`scan.go`) which walks that hierarchy and caches it via `store`. `status.go`'s `ResolveStatus` maps an abstract status type (e.g. "active"/"closed") to a list's actual ClickUp status name, using the cache with a 24h staleness check before hitting the API.
- **`internal/sync`** — `Worker` drains `sync_queue` rows written by `session.Service.enqueueSync`. Two modes: `Run` (ticker-driven background loop, used implicitly whenever `serve` is running since checkpoints/stops enqueue ops) and `DrainOnce` (single pass, used by `taskflow sync`). Failed ops are retried up to `max_retries`; ops are rate-limited via `SyncConfig.RateLimitMS` between each API call.
- **`internal/gitctx`** — captures branch/SHA/dirty-files for a project path; the sole implementation of `session.GitCapturer`.
- **`internal/cli`** — one file per Cobra subcommand, registered via `init()` on the package-level `rootCmd` in `root.go`. `PersistentPreRunE` on root loads config into the command context (`ConfigFromContext`); every subcommand opens its own `store.New(cfg.DBPath())` and closes it via `defer`.
- **`internal/server`** — MCP server (`mark3labs/mcp-go`). `registerTools()` maps each `tf_*` tool to a handler that parses MCP args and calls into the same `session.Service` methods the CLI uses. Adds a `clickupAdapter` and `gitCapturerImpl` mirroring the CLI's own adapters (currently duplicated rather than shared — be aware both `cli/start.go` and `server/server.go` define near-identical adapter structs).

### Session state machine

Status values: `ACTIVE`, `PAUSED`, `COMPLETED`, `ARCHIVED` (`internal/session/status.go`). Transitions are validated by `ValidateTransition`: `ACTIVE` and `PAUSED` can go anywhere; `COMPLETED` can only move to `COMPLETED` or `ARCHIVED`; `ARCHIVED` is terminal. `Service.Start` treats an existing `PAUSED` session for the same task as a resume (reuses session ID); `COMPLETED`/`ARCHIVED` sessions cause a new session to be created instead.

### Locking

A per-project advisory lock lives at `<project_path>/.b1codes/session.lock` (`internal/session/lock.go`), containing the active session ID. `Service.Start` refuses to start a *different* task in a project that already has an active session locked, unless the existing session isn't actually `ACTIVE` anymore.

### Agentic contract

`Service.BuildAgenticContract` assembles the JSON payload returned by `tf_start`/CLI `start` — recent checkpoints, unresolved snags, and cross-session "related resolutions" (snags with a matching normalized error signature resolved in *other* sessions/projects, found via `store.FindMatchingResolutions`). This is the primary mechanism for giving an LLM agent continuity across session starts/resumes.

## Design docs

`docs/superpowers/specs/` and `docs/superpowers/plans/` contain the original design spec and implementation plan this codebase was built from — useful background if extending the architecture significantly.
