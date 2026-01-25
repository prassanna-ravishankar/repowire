# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

```bash
# Install dependencies
pip install -e "."            # core (includes libtmux)
pip install -e ".[dev]"       # dev tools (pytest, ruff, ty)
pip install -e ".[relay]"     # relay server deps

# Run tests
pytest                        # all tests
pytest tests/test_config.py   # single test file
pytest -k "test_add_peer"     # single test by name

# Linting and type checking
ruff check repowire/          # lint
ruff format repowire/         # format
uv run ty check repowire/     # type check

# CI runs: ruff check, ty check, pytest (see .github/workflows/ci.yml)

# Start daemon (per-peer routing auto-detects backend)
repowire serve                # default: 127.0.0.1:8377
repowire serve --port 8080

# Setup (auto-detects and configures all available backends)
repowire setup --dev          # dev mode (uses local code)
repowire setup                # production mode
```

## Dashboard & Observability

Repowire includes a "Cyber-Minimalist Control Plane" dashboard for monitoring inter-agent communication.

- **URL**: `http://localhost:8377/dashboard` (when `repowire serve` is running)
- **Architecture**: Next.js static export served by the Python FastAPI daemon.
- **Event Logging**: The `PeerManager` maintains an in-memory circular buffer of the last 100 communication events (queries, responses, broadcasts).

### Dashboard Development

```bash
# Build the UI and export static files to web/out/
repowire build-ui

# Run frontend in development mode with hot reloading
cd web
npm run dev # runs on http://localhost:3000
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
│ - Uses libtmux          │     │ - WebSocket plugin      │
│ - Requires tmux_session │     │ - Plugin injects via SDK│
│ - Response via hooks    │     │ - Response via WebSocket│
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

Hooks in `~/.claude/settings.json` auto-register peers and manage state:

- **SessionStart** → `repowire hook session` → Registers peer, outputs `additionalContext` with peer list
- **SessionEnd** → `repowire hook session` → Marks peer offline
- **UserPromptSubmit** → `repowire hook prompt` → Marks peer as BUSY
- **Stop** → `repowire hook stop` → Extracts response from transcript, marks peer ONLINE
- **Notification** (idle_prompt) → `repowire hook notification` → Marks peer ONLINE after 60s idle (handles interrupt)

**Peer State Machine:** `OFFLINE → ONLINE ↔ BUSY` (SessionStart→ONLINE, UserPromptSubmit→BUSY, Stop/Notification→ONLINE, SessionEnd→OFFLINE)

Key files:
- `hooks/installer.py` - Installs/uninstalls hooks in `~/.claude/settings.json`
- `hooks/session_handler.py` - Handles SessionStart and SessionEnd events
- `hooks/prompt_handler.py` - Handles UserPromptSubmit (sets BUSY)
- `hooks/stop_handler.py` - Captures response from transcript, sends to daemon
- `hooks/notification_handler.py` - Handles idle_prompt (resets BUSY→ONLINE after interrupt)

### Configuration

File: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  # Per-peer routing auto-detects backend based on peer config

relay:  # Experimental - not usable yet
  enabled: false

opencode:
  default_url: "http://localhost:4096"

peers:
  frontend:
    name: frontend
    path: "/path/to/frontend"
    circle: "myteam"              # optional, defaults to tmux session name
    tmux_session: "0:frontend"    # for claudemux backend
    opencode_url: "http://..."    # for opencode backend
    session_id: "..."             # Claude session ID (set by hooks)
    metadata:
      branch: "main"              # git branch (auto-populated by SessionStart hook)
```


### Protocol (protocol/)

Message types: `QUERY`, `RESPONSE`, `NOTIFICATION`, `BROADCAST`

All messages have: `id`, `type`, `from_peer`, `to_peer`, `payload`, `correlation_id`, `timestamp`

Peer status: `ONLINE`, `BUSY`, `OFFLINE`

### Circles (Peer Isolation)

Circles are logical subnets that isolate groups of peers. Peers can only communicate within their circle unless explicitly allowed.

- **Default circle**: Derived from tmux session name
- **Set via CLI**: `repowire peer register --circle myteam`
- **Set via config**: Add `circle: myteam` to peer config
- **Bypass**: CLI commands bypass circle restrictions by default

### HTTP API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/health` | GET | Health check with backend info |
| `/peers` | GET | List all peers |
| `/peers` | POST | Register peer |
| `/peers/{name}` | DELETE | Unregister peer |
| `/peers/{name}/offline` | POST | Mark peer offline, cancel pending queries |
| `/query` | POST | Send query, wait for response |
| `/notify` | POST | Send notification (fire-and-forget) |
| `/broadcast` | POST | Send to all peers |
| `/session/update` | POST | Update peer session status |
| `/hook/response` | POST | Receive response from Stop hook (claudemux) |
| `/events` | GET | Get last 100 communication events |
| `/events/stream` | GET | SSE stream of real-time events |

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

## Integration Testing

Use the `/integration-test` skill for end-to-end testing. It supports three modes:

- **claudemux**: Test Claude Code sessions via tmux hooks
- **opencode**: Test OpenCode sessions via WebSocket plugin
- **mixed**: Test cross-backend communication (Claude Code ↔ OpenCode)

The skill guides you through environment discovery, pre-test teardown, test execution, and final cleanup.
