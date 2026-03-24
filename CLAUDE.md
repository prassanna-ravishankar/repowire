# CLAUDE.md

## Build & Test

```bash
uv tool install --force --reinstall .   # install globally (hooks run from installed package!)
uv sync --extra dev                     # dev deps (pytest, ruff, ty, httpx-ws)
pytest                                  # 222 tests
ruff check repowire/                    # lint
uv run ty check repowire/              # type check
```

CI runs: ruff check, ty check, pytest (`.github/workflows/ci.yml`).

Channel server deps: `cd repowire/channel && bun install`

## Releasing

Update `version` in `pyproject.toml`, commit, tag, push:
```bash
git tag v0.X.Y && git push origin main --tags
```
CI triggers PyPI publish from tags.

## Architecture

```
                    ┌─────────────────────────────┐
                    │   HTTP Daemon (daemon/app.py)│
                    │   FastAPI, :8377              │
                    │                              │
                    │   PeerRegistry               │
                    │   MessageRouter              │
                    │   QueryTracker               │
                    │   WebSocketTransport          │
                    └──────────┬───────────────────┘
                               │ WebSocket /ws
            ┌──────────────────┼──────────────────┐
            │                  │                  │
   Channel transport    Legacy transport     Other peers
   (Claude Code 2.1.80+)  (older Claude)    (OpenCode, Telegram)
            │                  │                  │
   channel/server.ts    hooks/ws-hook.py    telegram/bot.py
   (MCP stdio)          (tmux injection)    opencode plugin
```

The daemon is the single routing hub. It doesn't care how a peer connects — all peers speak the same WebSocket protocol. The transport layer is client-side only.

### Key modules

- `channel/server.ts` — **primary Claude Code transport**: MCP channel with reply tool, permission relay
- `daemon/peer_registry.py` — peer state, circle access, events, lazy_repair, ghost eviction
- `daemon/message_router.py` — routes queries/notifications/broadcasts via WebSocket
- `daemon/query_tracker.py` — correlation ID tracking, asyncio Futures (async-locked)
- `daemon/routes/` — HTTP endpoints (peers, messages, websocket, spawn, health)
- `mcp/server.py` — MCP tools (list_peers, ask_peer, notify_peer, etc.)
- `relay/server.py` — hosted relay at repowire.io (WS bridge + HTTP tunnel)
- `telegram/bot.py` — mobile mesh control via Telegram inline buttons
- `hooks/` — **legacy** Claude Code transport (session, stop, prompt, notification, websocket_hook)

## Transports

### Channel (primary — Claude Code v2.1.80+)

```
Claude Code ←stdio→ channel/server.ts ←WebSocket→ Daemon
```

- Messages arrive as `<channel source="repowire" from_peer="..." msg_type="...">` tags
- Queries include `correlation_id` — Claude calls the `reply` tool to respond
- Permission relay: forwards tool approval prompts to Telegram/dashboard
- Requires claude.ai login (not API/Console key)
- `repowire setup` auto-detects version and installs channel or hooks

How it works:
1. Claude Code spawns `server.ts` as MCP subprocess (stdio)
2. `server.ts` connects to daemon via WebSocket, registers as peer
3. Incoming messages → `notifications/claude/channel` → Claude sees `<channel>` tags
4. Claude replies via `reply` MCP tool → `server.ts` sends WS response → daemon resolves query

### Hooks (legacy — older Claude Code or API/Console auth)

```
Claude Code → hooks → websocket_hook.py ←WebSocket→ Daemon
             → stop hook → transcript parse → HTTP /response
```

- **SessionStart** → registers peer, spawns ws-hook (flock dedup), injects peer context
- **Stop** → extracts response + tool calls from transcript, posts chat turns, delivers responses
- **UserPromptSubmit** → marks BUSY
- **Notification** (idle_prompt) → resets ONLINE

In channel mode, only the Stop hook is kept (for dashboard chat_turn events).

Key files: `session_handler.py`, `stop_handler.py`, `prompt_handler.py`, `notification_handler.py`, `websocket_hook.py`, `utils.py`

### Setup auto-detection

`repowire setup` checks:
1. Claude Code version ≥ 2.1.80? → channel transport
2. `bun` runtime available? → channel transport
3. Otherwise → hooks transport (with clear message why)

`install_hooks(channel_mode=True)` installs only the Stop hook when using channel transport.

## Design Philosophy: Lazy Repair

Nothing polls. Work is deferred until needed, then piggy-backed on that request.

- **Liveness:** `lazy_repair()` runs max 1x/30s, triggered by MCP endpoints
- **Persistence:** Disk writes debounced via dirty flags, flushed during lazy_repair or shutdown
- **Rule:** Never add polling loops, periodic timers, or eager disk writes

## Config

File: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "optional"
  prune_max_age_hours: 24
  spawn:
    allowed_commands: [claude, claude --dangerously-skip-permissions]
    allowed_paths: [~/git, ~/projects]

relay:
  enabled: true
  url: "wss://repowire.io"
  api_key: "rw_..."
```

Channel config: `~/.claude.json` (user-level MCP servers) — managed by `repowire setup`.

## Relay

Hosted at repowire.io. Daemon connects outbound via WSS. Cookie-based auth for dashboard.

- `relay/server.py` — FastAPI relay (WS bridge + HTTP tunnel + SSE bridge)
- `daemon/relay_client.py` — outbound WSS with auto-reconnect (strips proxy headers)
- Deploy: `.github/workflows/relay.yml` → GCR → Helm → GKE

## Dashboard

- Next.js static export at `localhost:8377/dashboard`, remote at `repowire.io/dashboard`
- Events: 500-item circular buffer, persisted to `~/.repowire/events.json`
- Tool calls: stop hook extracts from transcript JSONL, included in `chat_turn` events
- Build: `repowire build-ui` or `cd web && npm run dev`

## Telegram Bot

```bash
TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
```

- `telegram/bot.py` — ~230 lines, zero extra deps
- Sticky routing: `/select peer` → all messages go there
- `@telegram` and `@dashboard` are human — context injection tells agents

## Testing Notes

- Route tests: `httpx.AsyncClient` + `ASGITransport`, manually init deps
- WebSocket tests: `httpx-ws` + `ASGIWebSocketTransport`
- Hooks run from installed package — `uv tool install --force --reinstall .` after changes
- 222 tests covering routes, WebSocket, auth, query tracker, hooks, config, transcript
