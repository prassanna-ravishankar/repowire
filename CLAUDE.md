# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Releasing

When bumping versions: update `version` in `pyproject.toml`, commit, then tag and push:

```bash
git tag v0.X.Y
git push origin main --tags
```

Always tag the commit that contains the version bump. CI triggers PyPI publish from tags.

## Build & Development Commands

```bash
# Install as global tool from local source (for dev/testing)
uv tool install --force --reinstall .

# Install dev dependencies (for running tests/linting)
uv sync --extra dev

# Run tests
pytest                        # all tests
pytest tests/test_config.py   # single test file
pytest -k "test_add_peer"     # single test by name

# Linting and type checking
ruff check repowire/          # lint
ruff format repowire/         # format
uv run ty check repowire/     # type check

# CI runs: ruff check, ty check, pytest (see .github/workflows/ci.yml)

# Start daemon
repowire serve                # default: 127.0.0.1:8377
repowire serve --port 8080

# Setup (auto-detects installed agent types)
repowire setup

# Spawn a new peer
repowire peer new ~/git/myproject --circle dev
```

## Dashboard & Observability

Repowire includes a "Cyber-Minimalist Control Plane" dashboard for monitoring inter-agent communication.

- **URL**: `http://localhost:8377/dashboard` (when `repowire serve` is running)
- **Architecture**: Next.js static export served by the Python FastAPI daemon.
- **Event Logging**: The `PeerManager` maintains an in-memory circular buffer of the last 500 communication events (queries, responses, broadcasts), persisted to `~/.repowire/events.json`.

### Dashboard Development

```bash
# Build the UI and export static files to web/out/
repowire build-ui

# Run frontend in development mode with hot reloading
cd web
npm run dev # runs on http://localhost:3000
```

## Spawning Peers

### CLI Command

```bash
repowire peer new [PATH] [options]
  --backend, -b    claude-code or opencode (default: claude-code)
  --command, -c    Custom command (default: claude/opencode)
  --circle         Circle name (default: "default")
```

### Core Module (`spawn.py`)

- `SpawnConfig` - Configuration dataclass (path, circle, backend, command)
- `SpawnResult` - Result dataclass (display_name, tmux_session)
- `spawn_peer(config)` - Creates tmux window, runs command
- `kill_peer(tmux_session)` - Kills tmux window
- `attach_session(tmux_session)` - Attaches to tmux session

### Behavior

- Circle maps to tmux session name
- Unique window names with numeric suffixes (myproject, myproject-2, ...)
- Graceful daemon registration (continues if daemon unavailable)

## Design Philosophy: Lazy Repair, Not Eager Polling

Repowire follows a "lazy repair" model throughout. Nothing polls on a timer. Work is deferred until someone actually needs the result, then piggy-backed on that request.

**Liveness:** Peers are not health-checked on a loop. Instead, `lazy_repair()` runs at most once per 30s, triggered only when an MCP-facing endpoint is hit (`/peers`, `/query`, `/notify`, `/broadcast`). If no one is asking, no one is checking. Dead peers are discovered when someone tries to talk to them.

**Persistence:** In-memory state (peer registry, event history, session mappings) is the source of truth at runtime. Disk writes are debounced via dirty flags and flushed during the lazy_repair cycle or at shutdown — never on every mutation. If the daemon crashes mid-cycle, at most 30s of state is lost, which is acceptable since peers re-register on reconnect.

**Hook lifecycle:** The WebSocket hook (`ws-hook`) is fully reactive — no polling loops. The daemon sends pings, the hook replies with pong + pane liveness. If the pane dies, the hook exits, the WebSocket disconnects, and the daemon marks the peer offline. No heartbeat timers, no file watchers.

**Implication for new code:** Never add polling loops, periodic timers, or eager disk writes. If you need something checked, piggy-back it on lazy_repair or on the specific request that needs the data. If you need something persisted, set a dirty flag and let the existing flush cycle handle it.

## Architecture Overview

Repowire is a mesh network enabling AI coding agents to communicate. All message delivery goes through a unified WebSocket protocol — the daemon treats all peers identically regardless of agent type.

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
│  endpoints. Uses PeerRegistry for routing via WebSocket.    │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│              PeerRegistry (daemon/peer_registry.py)          │
│  Single source of truth: peer state + identity persistence. │
│  Merges former PeerManager + SessionMapper. Atomic updates  │
│  to circle/display_name across both in-memory and disk.     │
└─────────────────────────────────────────────────────────────┘
         │              │                │
         ▼              ▼                ▼
  MessageRouter    QueryTracker    WebSocketTransport
  (routes msgs)    (correlation    (connection mgmt)
                    ID tracking,
                    async locked)
```

### Daemon Modules

- `daemon/peer_registry.py` - PeerRegistry: unified peer registry + identity persistence, circle access control, event tracking, lazy_repair liveness sweep, ghost eviction. Single source of truth for peer state — replaces former PeerManager + SessionMapper split.
- `daemon/message_router.py` - MessageRouter: routes queries/notifications/broadcasts via WebSocket
- `daemon/query_tracker.py` - QueryTracker: correlation ID tracking, asyncio Futures for request/response. Protected by asyncio.Lock.
- `daemon/websocket_transport.py` - WebSocketTransport: connection lifecycle, ping/pong liveness
- `daemon/auth.py` - Authentication middleware (optional token-based)
- `daemon/deps.py` - FastAPI dependency injection (`get_peer_registry()`)
- `daemon/routes/websocket.py` - Unified `/ws` endpoint for all agent types
- `daemon/routes/peers.py` - Peer CRUD endpoints
- `daemon/routes/messages.py` - Query/notify/broadcast endpoints

### Message Flow: Query

```
1. MCP tool ask_peer() → HTTP POST /query
2. PeerManager routes via WebSocket to target peer
3. Peer's hook/plugin processes query
4. Response sent back via WebSocket
5. Response returned to caller
```

### Hooks System (Claude Code)

Hooks in `~/.claude/settings.json` auto-register peers and manage state:

- **SessionStart** → `repowire hook session` → Registers peer, spawns ws-hook (with flock dedup to skip ephemeral sub-sessions), outputs `additionalContext` with peer list
- **SessionEnd** → `repowire hook session` → No-op (fires spuriously between turns)
- **UserPromptSubmit** → `repowire hook prompt` → Marks peer as BUSY
- **Stop** → `repowire hook stop` → Extracts response from transcript, pops pending `correlation_id` from `pending-{pane}.json`, delivers to daemon via `POST /response` (with correlation_id if available), marks peer ONLINE
- **Notification** (idle_prompt) → `repowire hook notification` → Marks peer ONLINE after 60s idle (handles interrupt)

**Peer State Machine:** `OFFLINE → ONLINE ↔ BUSY` (SessionStart→ONLINE, UserPromptSubmit→BUSY, Stop/Notification→ONLINE, WS disconnect→OFFLINE)

**WebSocket Hook Lifecycle:**
- SessionStart spawns a `websocket_hook.py` background process (new WS connect replaces old connection atomically in daemon)
- SessionEnd does nothing — fires spuriously between turns, so marking offline here would cancel valid in-flight queries
- The ws-hook is fully reactive (no polling). Daemon sends `ping`, ws-hook replies `pong` with pane liveness. If pane is dead on ping, ws-hook exits via `os._exit(0)`
- On exit: WebSocket disconnect triggers daemon to mark peer OFFLINE

**Lazy Repair (Daemon-Driven Liveness):**
- `PeerManager.lazy_repair()` runs max 1x per 30s, triggered by MCP-facing endpoints (`/query`, `/notify`, `/broadcast`, `/peers`)
- Pings all ONLINE/BUSY peers via WebSocket. Dead peers (no pong within 5s) get marked OFFLINE
- OpenCode peers skip ping — active WS connection is sufficient proof of liveness
- Replaces the old ws-hook pane liveness checker with daemon-side control

**Pane-Based Routing:**
- Hooks use `pane_id` (from `$TMUX_PANE`) instead of file-based session_id lookup for status updates
- `/session/update` accepts either `peer_name` or `pane_id`
- `/peers/by-pane/{pane_id}` endpoint for MCP `whoami` and `_get_my_peer_name()`
- Display name passed to ws-hook via `REPOWIRE_DISPLAY_NAME` env var (no file artifacts)

**Zero File Artifacts:**
- No per-pane files written (`.pid`, `.sid`, `.name`, `.uname` all removed)
- No correlation or response directories — stop hook delivers responses via `POST /response`
- `repowire setup` cleans up legacy artifacts from pre-0.4.3 installs

**Tmux Text Injection Pattern (Gastown NudgeSession):**

`tmux send-keys -l` triggers bracketed paste but does NOT send the closing `ESC[201~`. The TUI stays in paste mode and swallows subsequent keystrokes including Enter. The fix (validated empirically, matches Gastown's battle-tested NudgeSession implementation) is a 500ms debounce, then Escape (exits vim INSERT mode, harmless otherwise), then Enter:

```python
# 1. Send text literally (triggers bracketed paste)
subprocess.run(["tmux", "send-keys", "-t", pane_id, "-l", text])
# 2. 500ms debounce — tested, required for paste to complete
time.sleep(0.5)
# 3. Escape — exits vim INSERT mode if active, harmless otherwise
subprocess.run(["tmux", "send-keys", "-t", pane_id, "Escape"])
time.sleep(0.1)
# 4. Enter to submit
subprocess.run(["tmux", "send-keys", "-t", pane_id, "Enter"])
```

**Unique Peer Names:**

Each Claude session's `display_name` is the first 8 chars of Claude's `session_id` (e.g. `00893aaf`). The same session always gets the same name across resumes/clears/compacts. A fresh `claude` invocation (new `session_id`) gets a new name. The folder name is stored as `metadata.project` for human context.

Key files:
- `installers/claude_code.py` - Installs/uninstalls hooks in `~/.claude/settings.json`
- `hooks/session_handler.py` - Handles SessionStart and SessionEnd events. Uses flock for ws-hook dedup.
- `hooks/prompt_handler.py` - Handles UserPromptSubmit (sets BUSY)
- `hooks/stop_handler.py` - Captures response + tool calls from transcript, posts chat turns via `POST /events/chat`, delivers query response via `POST /response` (with correlation_id from pending file)
- `hooks/notification_handler.py` - Handles idle_prompt (resets BUSY→ONLINE after interrupt)
- `hooks/websocket_hook.py` - Persistent WebSocket connection for query/response delivery. Writes pending correlation_ids to `pending-{pane}.json` for stop hook.
- `hooks/utils.py` - Shared utilities including `derive_display_name()` (single source of truth for display name derivation)

### Security

**WebSocket Authentication (Optional)**

To prevent unauthorized WebSocket connections to the daemon, you can enable authentication:

1. Add `auth_token` to your config (`~/.repowire/config.yaml`):
```yaml
daemon:
  auth_token: "your-secret-token-here"
```

2. For OpenCode peers, set the environment variable before starting OpenCode:
```bash
export REPOWIRE_AUTH_TOKEN="your-secret-token-here"
opencode  # or your preferred launcher
```

**Spawn Allowlist (MCP spawn_peer / kill_peer)**

By default, spawn via MCP is disabled. To allow agents to spin up new sessions programmatically, both `allowed_commands` and `allowed_paths` must be non-empty in `~/.repowire/config.yaml`:

```yaml
daemon:
  spawn:
    allowed_commands:
      - claude
      - claude --dangerously-skip-permissions
      - opencode
    allowed_paths:
      - ~/git
      - ~/projects
```

MCP tools `spawn_peer` and `kill_peer` call `POST /spawn` and `POST /kill` on the daemon. The daemon validates:
- `command` — exact string match against `allowed_commands`
- `path` — must exist on disk and be under one of the `allowed_paths` roots

`kill` only works on sessions previously spawned by this daemon instance (tracked in-memory). `repowire setup` prints a reminder about this config.

Key files:
- `repowire/daemon/routes/spawn.py` — `/spawn` and `/kill` endpoints
- `repowire/config/models.py` — `SpawnSettings`, `DaemonConfig.spawn`
- `repowire/mcp/server.py` — `spawn_peer` and `kill_peer` MCP tools
- `repowire/spawn.py` — underlying tmux spawn/kill logic (unchanged)

**CORS Protection**

The daemon restricts CORS to localhost origins only (`http://localhost:3000`, `http://127.0.0.1:3000`, `http://localhost:8377`, `http://127.0.0.1:8377`) to prevent CSRF attacks from malicious websites. When relay is enabled, `https://repowire.io` and `https://relay.repowire.io` are also allowed.

### Hosted Relay (`relay/`)

The hosted relay at `relay.repowire.io` enables cross-machine agent meshing and remote dashboard access.

**Architecture:**
```
Browser → repowire.io → enter key → relay proxies via WSS → local daemon → dashboard
Daemon A ←WSS→ relay.repowire.io ←WSS→ Daemon B (cross-machine mesh)
```

**How it works:**
- `repowire serve --relay` connects the local daemon to the relay via outbound WebSocket
- The relay bridges messages between daemons (daemon-to-daemon, NOT peer-to-relay)
- HTTP tunnel: browser requests to `/d/{api_key}/...` are proxied through the WSS tunnel to the local daemon
- Auth: stateless HMAC-SHA256 keys (`rw_{user_id}_{signature}`), validated via `REPOWIRE_RELAY_SECRET` env var

**Key files:**
- `repowire/relay/server.py` — FastAPI relay server (WebSocket bridge + HTTP tunnel + landing page)
- `repowire/relay/auth.py` — Stateless HMAC API key generation/validation
- `repowire/daemon/relay_client.py` — Outbound WSS client with auto-reconnect + HTTP tunnel handler

**Deployment:**
- GKE Autopilot (Spot Pods), namespace `repowire`
- Gateway API HTTPRoute serves both `repowire.io` and `relay.repowire.io`
- CI/CD: `.github/workflows/relay.yml` (GitHub Actions → GCR → Helm)
- Image: `us-docker.pkg.dev/baldmaninc/gcr.io/repowire-relay`

### Configuration

File: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  # Security (optional): WebSocket authentication
  auth_token: "your-secret-token-here"  # Optional: require auth for WebSocket connections
  # Spawn (optional): allow MCP spawn_peer to create sessions
  spawn:
    allowed_commands:          # both lists must be non-empty for spawn to be enabled
      - claude
      - claude --dangerously-skip-permissions
    allowed_paths:
      - ~/git
      - ~/projects

relay:
  enabled: true
  url: "wss://relay.repowire.io"
  api_key: "rw_..."  # Auto-generated on first `repowire serve --relay`

peers:
  frontend:
    name: frontend
    path: "/path/to/frontend"
    circle: "myteam"              # optional, defaults to tmux session name
    tmux_session: "0:frontend"    # for Claude Code peers
    metadata:
      branch: "main"              # git branch (auto-populated by SessionStart hook)
```


### Protocol (protocol/)

Message types: `query`, `response`, `notify`, `broadcast`, `status`, `error`, `ping`, `pong`

WebSocket messages use: `type`, `correlation_id`, `from_peer`, `text`

Peer status: `ONLINE`, `BUSY`, `OFFLINE`

`DEFAULT_QUERY_TIMEOUT` (`config/models.py`): 300s (5 min). Used by CLI, daemon API, and message router. For long-running queries, prefer `notify_peer` (fire-and-forget) over `ask_peer` (blocking).

### Key Types

**AgentType** (`config/models.py`): Enum with `CLAUDE_CODE = "claude-code"`, `OPENCODE = "opencode"`

**PeerStatus** (`protocol/peers.py`): Enum with `ONLINE`, `BUSY`, `OFFLINE`

**Peer Identity:**
- Primary: `peer_id` (daemon-assigned: `repow-{circle}-{uuid8}`, e.g., `repow-dev-a1b2c3d4`)
- Secondary: `display_name` (folder name, for human-friendly addressing)

**Status Symbols (TUI):**
- `●` online (green)
- `◉` busy (yellow)
- `○` offline (dim)

### Circles (Peer Isolation)

Circles are logical subnets that isolate groups of peers. Peers can only communicate within their circle unless explicitly allowed.

- **Default circle**: Derived from tmux session name
- **Set via CLI**: `repowire peer register --circle myteam`
- **Set via config**: Add `circle: myteam` to peer config
- **Bypass**: CLI commands bypass circle restrictions by default

### HTTP API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/health` | GET | Health check |
| `/peers` | GET | List all peers |
| `/peers` | POST | Register peer |
| `/peers/{identifier}` | GET | Get peer by session_id or display_name (`?circle=` to disambiguate) |
| `/peers/{name}` | DELETE | Unregister peer (`?circle=` to disambiguate) |
| `/peers/by-pane/{pane_id}` | GET | Get peer by tmux pane ID |
| `/peers/{name}/offline` | POST | Mark peer offline, cancel pending queries |
| `/peers/{name}/description` | POST | Set peer's task description |
| `/peers/circle` | POST | Set peer's circle |
| `/events/chat` | POST | Ingest chat turn from stop hook |
| `/query` | POST | Send query, wait for response |
| `/notify` | POST | Send notification (fire-and-forget) |
| `/broadcast` | POST | Send to all peers |
| `/session/update` | POST | Update peer session status (by `peer_name` or `pane_id`) |
| `/response` | POST | Deliver response from stop hook (by `pane_id`) |
| `/events` | GET | Get last 500 communication events |
| `/events/stream` | GET | SSE stream of real-time events |

## Key Design Decisions

1. **Unique peer names** - First 8 chars of Claude's `session_id` (e.g. `00893aaf`). Stable across session resumes; new invocation = new name. OpenCode uses 8 chars after `ses` prefix of `session.id`. Folder name stored as `metadata.project`.
2. **Correlation IDs** - UUID-based request/response matching via asyncio Futures
3. **In-memory peer registry** - PeerRegistry is the single source of truth at runtime. Disk persistence is debounced (see Design Philosophy above).
4. **Peer validation** - WebSocket connect validates display_name and circle format
5. **HTTP response delivery** - Stop hook POSTs response to daemon via `/response` with `correlation_id` (from pending file) when available; falls back to oldest pending query for that peer
6. **Peer metadata** - Includes `project` (folder name) and git branch, auto-populated by SessionStart hook
7. **Context injection** - SessionStart hook outputs `additionalContext` with peer list for Claude
8. **TSV MCP output** - `list_peers` and `whoami` return TSV (more token-efficient than JSON for agents)
9. **Ghost eviction** - `register_peer` evicts OFFLINE peers with same (display_name, backend) regardless of circle, cleaning up stale registrations from dead ws-hooks
10. **Circle-preferred `from_peer` lookup** - `from_peer` is resolved preferring the target peer's circle first, preventing false circle boundary errors when sender name appears in multiple circles
11. **Event persistence** - Events persisted to `~/.repowire/events.json`, loaded on startup. Writes debounced via lazy_repair (see Design Philosophy). Deque maxlen=500 caps memory.
12. **Tool call extraction** - Stop hook extracts `tool_use` entries from Claude's transcript JSONL and includes them as `tool_calls` in `chat_turn` events. Dashboard renders them as collapsible lists per assistant turn.
13. **SSE bridge** - Relay serves `/events/stream` SSE endpoint. Shared poller fans out to all connected browser clients. Dashboard shows "Connected"/"Disconnected" based on SSE state.
14. **Dashboard identity** - Messages from `@dashboard` are from a human at the web control plane. Context injection tells agents to treat them as direct user instructions.

## Testing

- Framework: pytest with pytest-asyncio (auto mode)
- Tests use `tempfile` and `unittest.mock` extensively
- Route tests use `httpx.AsyncClient` with `ASGITransport` and manually initialized deps (no lifespan)
- WebSocket tests use `httpx-ws` with `ASGIWebSocketTransport`
- Important: `uv tool install --force --reinstall .` is required after code changes for hooks to pick up new code (hooks run from installed package, not source)
- 222 tests covering: routes, WebSocket, auth, query tracker, message router, peer registry, hooks, config, transcript, spawn

## Integration Testing

Use the `/integration-test` skill for end-to-end testing. It supports three modes:

- **claude-code**: Test Claude Code sessions via tmux hooks
- **opencode**: Test OpenCode sessions via WebSocket plugin
- **mixed**: Test cross-agent-type communication (Claude Code ↔ OpenCode)

The skill guides you through environment discovery, pre-test teardown, test execution, and final cleanup.
