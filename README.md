# Taskflow

Taskflow is a developer productivity tool and context engine that connects your local coding environment with ClickUp tasks. It runs both as a standard command-line interface (CLI) and as a Model Context Protocol (MCP) server.

## Features

- **Session Tracking**: Start, checkpoint, pause, resume, and stop task-aligned coding sessions.
- **Git Context Capturing**: Automatically capture git status, active branch, and commit diff context.
- **Dynamic ClickUp Sync**: Map task statuses dynamically and enqueue progress comments asynchronously.
- **MCP Server Protocol**: Expose standard tools to LLM models for seamless agent integration.
- **Offline Resilience**: Maintain a WAL-mode SQLite database with a rate-limited retry worker to guarantee sync delivery.

## Installation & Setup

1. **Build taskflow**:
   ```bash
   go build ./cmd/taskflow
   ```

2. **Configure environment**:
   Create a configuration file or set environment variables:
   ```bash
   export CLICKUP_API_KEY="your-api-key"
   ```

3. **Initialize workspace**:
   Create and scan ClickUp workspace:
   ```bash
   ./taskflow init
   ```

## CLI Usage

### Start a Session
```bash
./taskflow start <task_id> <project_path>
```

### Save progress (checkpoint)
```bash
./taskflow checkpoint <session_id> "Refactored user router" --files router.go,main.go
```

### Log snags and blockers
```bash
./taskflow snag <session_id> "go build failed: undefined compiler type" --category build
```

### List sessions
```bash
./taskflow list --status active,paused
```

### Stop/pause a session
```bash
./taskflow stop <session_id> --summary "Completed Phase 4 task details" --status completed
```

### Run MCP Server
```bash
./taskflow serve
```

## Architecture

Taskflow is composed of:
- `cli`: Cobra commands that control the state machine.
- `session`: Service layer orchestrating the state machine, advisory locks, and domain logic.
- `clickup`: Client library interacting with ClickUp API and hierarchical scanning/caching.
- `store`: SQLite schema and data retrieval layer.
- `sync`: Rate-limited async worker dequeuing/retrying sync queue operations.
- `server`: Stdio MCP server registering and handling tool invokes.