# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

```bash
# Install dependencies (use --all for both backends)
pip install -e ".[all]"       # claudemux + opencode
pip install -e ".[claudemux]" # tmux backend only
pip install -e ".[opencode]"  # opencode backend only
pip install -e ".[dev]"       # dev tools (pytest, ruff, mypy)

# Run tests
pytest                        # all tests
pytest tests/test_config.py   # single test file
pytest -k "test_add_peer"     # single test by name

# Linting and type checking
ruff check repowire/          # lint
ruff format repowire/         # format
mypy repowire/                # type check

# Start daemon
repowire serve                # default: 127.0.0.1:8377
repowire serve --backend opencode --port 8080

# Setup (installs hooks + MCP server)
repowire setup --dev          # dev mode (uses local code)
repowire setup --backend claudemux
```

## Architecture Overview

Repowire is a mesh network enabling Claude Code sessions to communicate. It has a **pluggable backend architecture** supporting both local (tmux) and remote (OpenCode SDK) message delivery.

### Core Components

```
┌─────────────────────────────────────────────────────────────┐
│                     MCP Server (mcp/server.py)              │
│  Thin HTTP client exposing list_peers, ask_peer, notify_peer│
│  broadcast tools. Delegates all work to daemon via HTTP.    │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  HTTP Daemon (daemon/app.py)                 │
│  FastAPI server with /query, /notify, /broadcast, /peers    │
│  endpoints. Uses PeerManager for routing.                   │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  PeerManager (daemon/core.py)                │
│  Central routing. Validates backend requirements, formats   │
│  messages, delegates to configured backend.                 │
└─────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              ▼                               ▼
┌─────────────────────────┐     ┌─────────────────────────┐
│   ClaudemuxBackend      │     │    OpencodeBackend      │
│   (backends/claudemux/) │     │   (backends/opencode/)  │
│                         │     │                         │
│ - Uses libtmux          │     │ - Uses opencode-ai SDK  │
│ - Requires tmux_session │     │ - Requires opencode_url │
│ - Response via hooks    │     │ - Direct SDK response   │
└─────────────────────────┘     └─────────────────────────┘
```

### Backend Interface (backends/base.py)

All backends implement:
- `send_message(peer, text)` - Fire-and-forget
- `send_query(peer, text, timeout)` - Wait for response
- `get_peer_status(peer)` - Check online/offline
- `install()` / `uninstall()` / `check_installed()`

### Message Flow: Query with Claudemux Backend

```
1. MCP tool ask_peer() → HTTP POST /query
2. PeerManager formats: "@{from_peer} asks: {text}"
3. Backend creates pending file: ~/.repowire/pending/{tmux_session}.json
4. Backend sends to tmux pane via libtmux
5. Claude processes, responds
6. Stop hook fires → reads transcript → extracts last assistant response
7. Stop hook sends to daemon via HTTP POST /hook/response
8. Backend resolves asyncio.Future with response
9. Response returned to caller
```

### Hooks System (claudemux only)

Hooks in `~/.claude/settings.json` auto-register peers and capture responses:

- **SessionStart** → `repowire hook session` → Registers peer (name = folder name, git branch in metadata), outputs `additionalContext` with peer list
- **SessionEnd** → `repowire hook session` → Clears session_id
- **Stop** → `repowire hook stop` → Extracts last assistant response from transcript, sends via HTTP POST `/hook/response`

Key files:
- `hooks/installer.py` - Installs/uninstalls hooks in `~/.claude/settings.json`
- `hooks/session_handler.py` - Handles both SessionStart and SessionEnd events
- `hooks/stop_handler.py` - Captures response from transcript JSONL, sends to daemon

### Configuration

File: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  backend: "claudemux"  # or "opencode"

relay:
  enabled: false
  url: "wss://relay.repowire.io"
  api_key: null

opencode:
  default_url: "http://localhost:4096"

peers:
  frontend:
    name: frontend
    path: "/path/to/frontend"
    tmux_session: "0:frontend"    # for claudemux backend
    opencode_url: "http://..."    # for opencode backend
    session_id: "..."             # Claude session ID (set by hooks)
    metadata:
      branch: "main"              # git branch (auto-populated by SessionStart hook)
```

Environment overrides: `REPOWIRE_RELAY_URL`, `REPOWIRE_API_KEY`

### Protocol (protocol/)

Message types: `QUERY`, `RESPONSE`, `NOTIFICATION`, `BROADCAST`

All messages have: `id`, `type`, `from_peer`, `to_peer`, `payload`, `correlation_id`, `timestamp`

Peer status: `ONLINE`, `BUSY`, `OFFLINE`

### HTTP API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/health` | GET | Health check with backend info |
| `/peers` | GET | List all peers |
| `/peers` | POST | Register peer |
| `/peers/{name}` | DELETE | Unregister peer |
| `/query` | POST | Send query, wait for response |
| `/notify` | POST | Send notification (fire-and-forget) |
| `/broadcast` | POST | Send to all peers |
| `/session/update` | POST | Update peer session status |
| `/hook/response` | POST | Receive response from Stop hook (claudemux) |

## Key Design Decisions

1. **Peer name = folder name** - Auto-derived from cwd in SessionStart hook
2. **Correlation IDs** - UUID-based request/response matching via pending files
3. **Config reloaded per request** - Fresh peer discovery without daemon restart
4. **Backend validation** - Claudemux requires `tmux_session`, OpenCode requires `opencode_url`
5. **HTTP hooks** - Stop hook sends responses via HTTP POST to daemon's `/hook/response` endpoint
6. **Peer metadata** - Includes git branch info, auto-populated by SessionStart hook
7. **Context injection** - SessionStart hook outputs `additionalContext` with peer list for Claude

## Testing

- Framework: pytest with pytest-asyncio (auto mode)
- Tests use `tempfile` and `unittest.mock` extensively
- No integration tests yet (directory exists at `tests/integration/`)

## Dev Workflow: Integration Testing (Claudemux)

This workflow tests the **claudemux backend** - tmux-based peer communication for Claude Code sessions. This is the primary local development workflow.

> **Note:** The opencode backend has a simpler flow since the SDK returns responses directly (no hooks/tmux needed). For opencode testing, peers just need `opencode_url` configured and an OpenCode server running.

### Prerequisites

1. **Ask the user for two test projects** - Need two separate git repos/folders to use as test peers. Example prompt:
   > "I need two project directories to test repowire peer communication. Which two folders should I use? (They'll each run a Claude Code session that can talk to each other)"

2. **Ensure claudemux backend is set up**:
   ```bash
   repowire setup --dev --backend claudemux
   ```

### Step-by-Step Workflow

#### 1. Check/Create tmux Session

```bash
# Check if tmux is running
tmux list-sessions 2>/dev/null || echo "No tmux sessions"

# If no session exists, create one
tmux new-session -d -s repowire-test

# Attach to existing or new session
tmux attach -t repowire-test
```

#### 2. Create Test Windows

```bash
# Create two windows for test peers (from within tmux or using tmux commands)
tmux new-window -t repowire-test -n alice
tmux new-window -t repowire-test -n bob
```

#### 3. Start the Daemon

In a separate terminal or tmux pane:
```bash
repowire serve
# or for dev: uv run repowire serve
```

Verify it's running:
```bash
curl -s http://127.0.0.1:8377/health
# Should return: {"status":"ok","version":"0.1.0","backend":"claudemux",...}
```

#### 4. Launch Claude Sessions in Each Window

**Window "alice":**
```bash
tmux send-keys -t repowire-test:alice "cd /path/to/project-a && claude" Enter
```

**Window "bob":**
```bash
tmux send-keys -t repowire-test:bob "cd /path/to/project-b && claude" Enter
```

The SessionStart hook will auto-register each peer using the folder name.

#### 5. Verify Peer Registration

```bash
repowire peer list
```

Expected output shows both peers with status "online":
```
┌─────────────────────────────────────────────────┐
│ Name       │ Status │ Tmux Session       │ Path │
├─────────────────────────────────────────────────┤
│ project-a  │ online │ repowire-test:alice│ ...  │
│ project-b  │ online │ repowire-test:bob  │ ...  │
└─────────────────────────────────────────────────┘
```

#### 6. Test Communication

**Option A: CLI test**
```bash
repowire peer ask project-b "What is this project about?"
```

**Option B: From within a Claude session**
In alice's Claude session, type:
> "Ask project-b what their main API endpoints are"

Claude will use the `ask_peer` MCP tool to query the other session.

#### 7. Collaboration Test

Give both sessions a shared task to verify bidirectional communication:

In alice's session:
> "You're working with a peer called 'project-b'. Ask them what dependencies they use, then tell them what dependencies you use. Coordinate to identify any shared libraries."

### Cleanup

**Important:** Use `tmux kill-window` or `tmux kill-pane` to quit Claude sessions. Sending Ctrl+C via `tmux send-keys` is unreliable and often doesn't work properly.

```bash
# Kill test windows (preferred method to quit Claude sessions)
tmux kill-window -t repowire-test:alice
tmux kill-window -t repowire-test:bob

# Alternative: kill just the pane if window has multiple panes
# tmux kill-pane -t repowire-test:alice

# Stop daemon
repowire daemon stop
# or: curl -X POST http://127.0.0.1:8377/shutdown

# Unregister peers (optional - they'll be cleaned up automatically)
repowire peer unregister project-a
repowire peer unregister project-b
```

### Troubleshooting (Claudemux)

| Issue | Check |
|-------|-------|
| Peers not showing up | Verify hooks installed: `repowire claudemux status` |
| "No tmux session" error | Claude must run inside tmux, not a regular terminal |
| Query timeout | Check daemon running: `curl http://127.0.0.1:8377/health` |
| Wrong peer name | Peer name = folder name, not tmux window name |
| Hook not firing | Check `~/.claude/settings.json` has repowire hooks |

### Quick Verification Script (Claudemux)

```bash
#!/bin/bash
# quick-test-claudemux.sh - Verifies claudemux backend functionality

echo "=== Checking daemon ==="
curl -s http://127.0.0.1:8377/health | jq . || echo "ERROR: Daemon not running!"

echo "=== Checking hooks ==="
repowire claudemux status

echo "=== Listing peers ==="
repowire peer list

echo "=== Checking pending queries ==="
ls -la ~/.repowire/pending/ 2>/dev/null || echo "No pending queries"
```
