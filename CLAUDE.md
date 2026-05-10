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

**Versioning rules:**
- Patch (0.x.Y): bug fixes, cleanup, small additions
- Minor (0.X.0): significant new features or breaking changes
- After 0.9.x → 0.10.0, 0.11.0, etc. **Never auto-increment to 1.0.0**
- 1.0.0 is an intentional decision by Prass, not an automatic bump

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
   Hooks transport      Channel transport     Other peers
   (default)            (experimental)       (OpenCode, Telegram, Slack)
            │                  │                  │
   hooks/ws-hook.py    channel/server.ts    telegram/bot.py
   (tmux injection)    (MCP stdio)          slack/bot.py, opencode
```

The daemon is the single routing hub. It doesn't care how a peer connects — all peers speak the same WebSocket protocol. The transport layer is client-side only.

### Key modules

- `channel/server.ts` — **experimental** Claude Code transport: MCP channel with reply tool, permission relay
- `daemon/peer_registry.py` — peer state, circle access, events, lazy_repair, ghost eviction
- `daemon/message_router.py` — sends `type=ask` / `notify` / `broadcast` over WebSocket; legacy `send_query` for /query shim
- `daemon/ask_tracker.py` — slim ask state store: register, close, get, pending_for_peer. Open asks reappear in every Stop hook poll until acked.
- `daemon/query_tracker.py` — legacy /query correlation ID tracking, asyncio Futures (async-locked)
- `daemon/routes/asks.py` — `/ask`, `/ack`, `/asks/pending`. `/asks/{cid}/picked_up` + `/asks/{cid}/mark_reminded` are deprecated no-op 200 endpoints kept for one release for transport compat.
- `daemon/routes/` — other HTTP endpoints (peers, messages, websocket, spawn, health)
- `mcp/server.py` — MCP tools (list_peers, ask, ack, notify_peer, broadcast, whoami, set_description, spawn_peer, kill_peer)
- `relay/server.py` — hosted relay at repowire.io (WS bridge + HTTP tunnel)
- `telegram/bot.py` — mobile mesh control via Telegram inline buttons
- `hooks/` — **default** Claude Code transport (session, stop, prompt, notification, websocket_hook)
- `slack/bot.py` — Slack bot peer via Socket Mode

## Transports

### Hooks (default)

```
Claude Code → hooks → websocket_hook.py ←WebSocket→ Daemon
             → stop hook → transcript parse → HTTP /response (legacy /query)
                                            → /asks/pending  (reminder fetch)
```

- **SessionStart** → registers peer, spawns ws-hook (flock dedup), injects peer context
- **Stop** →
  1. extracts response + tool calls from transcript, posts chat turns
  2. delivers legacy /query response (single-purpose `pending-query-{pane}.json` FIFO)
  3. fetches `/asks/pending` → if open asks present, emits `{"decision": "block", "reason": <reminder>}` so Claude continues with the reminder as context (Stop event doesn't accept additionalContext)
  4. then `update_status(online)`
- **UserPromptSubmit** → marks BUSY (Claude Code only)
- **Notification** (idle_prompt) → resets ONLINE
- **ws-hook** → on `type=ask` arrival, injects `[ask #cid]` framed text. Daemon doesn't track pickup; the Stop hook reminder is the only way an un-acked ask is resurfaced.

In channel mode, only the Stop hook is kept (for dashboard chat_turn events).
`install_hooks(channel_mode=True)` installs only the Stop hook when using channel transport.

Key files: `session_handler.py`, `stop_handler.py`, `prompt_handler.py`, `notification_handler.py`, `websocket_hook.py`, `ask_lifecycle.py`, `utils.py`

### Hook Adapter (`hooks/adapters.py`)

Each agent runtime uses different hook event names and response fields. The adapter normalizes them so handlers are agent-agnostic:

| Concept | Claude Code | Codex | Gemini |
|---------|-------------|-------|--------|
| Prompt event | `UserPromptSubmit` | `UserPromptSubmit` | `BeforeAgent` |
| Stop event | `Stop` | `Stop` | `AfterAgent` |
| Response field | transcript JSONL | `last_assistant_message` | `prompt_response` |
| Hook output | empty | empty | `{"decision": "allow"}` |

`adapters.normalize()` maps all variants to canonical names. `hook_output()` prints the required stdout.

### MCP Server Identity (`mcp/server.py`)

The MCP server needs to know its own peer_id for `from_peer` in tool calls. Key behaviors:
- `_ensure_registered()` runs on every MCP tool call (lazy, idempotent)
- Backend detection: `GEMINI_CLI` env var for Gemini, `.codex/` in PATH for Codex, else claude-code
- Codex fires SessionStart late (after first interaction, not at startup). The MCP lazy registration covers this gap.
- `_get_my_peer_name()` caches peer name from pane-based daemon lookup, falls back to cwd folder name
- Tmux pane fallback: `get_pane_id()` tries `TMUX_PANE` env var, then `tmux display-message` (guarded by `TMUX` env to prevent false positives from non-tmux terminals)

### Channel (experimental - `repowire setup --experimental-channels`)

```
Claude Code <stdio> channel/server.ts <WebSocket> Daemon
```

- Messages arrive as `<channel source="repowire" from_peer="..." msg_type="...">` tags
- Queries (legacy /query shim) include `correlation_id` — Claude calls the `reply` tool to respond
- Asks (`msg_type=ask`) include `correlation_id` (and optional `reply_to`) — Claude calls the `ack(correlation_id, message?)` tool to close. Open asks reappear in every Stop hook reminder until acked.
- Permission relay: forwards tool approval prompts to Telegram/dashboard
- Requires claude.ai login (not API/Console key), Claude Code v2.1.80+, bun runtime
- Opt-in only: `repowire setup --experimental-channels`

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
- File uploads: 📎 button in compose bar, uploads to `POST /attachments`, path included in notification
- Build: `repowire build-ui` or `cd web && npm run dev`

### Dashboard Design System

See [`docs/design-system.md`](docs/design-system.md) for the full spec (color tokens, Tailwind config, responsive layout, component patterns).

## Attachments

- `daemon/routes/attachments.py` — `POST /attachments` (upload, 10MB limit) + `GET /attachments/{id}` (download)
- Storage: `~/.repowire/attachments/` with 24h TTL auto-cleanup
- Telegram: bot downloads photos, uploads to daemon, includes path in notification
- Dashboard: compose bar has file upload, tunneled through relay
- Claude reads images via Read tool (multimodal) using the local file path
- Relay tunnel: `/attachments` in `_TUNNEL_PREFIXES`, WS `max_size=16MB` for base64 payloads

## Telegram Bot

```bash
TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
```

- `telegram/bot.py` - zero extra deps
- Sticky routing: `/select peer` then all messages go there
- `@telegram` and `@dashboard` are human - context injection tells agents

## Slack Bot

```bash
SLACK_BOT_TOKEN=xoxb-... SLACK_APP_TOKEN=xapp-... SLACK_CHANNEL_ID=C... repowire slack start
```

- `slack/bot.py` - Socket Mode (no public URL), zero extra deps
- Sticky routing, Block Kit buttons for peer selection

## Agent Types

Four supported runtimes, all use the same hooks + MCP pattern:

| Agent | Backend enum | Installer | Hook config location |
|-------|-------------|-----------|---------------------|
| Claude Code | `claude-code` | `installers/claude_code.py` | `~/.claude/settings.json` |
| Codex | `codex` | `installers/codex.py` | `~/.codex/hooks.json` + `config.toml` |
| Gemini | `gemini` | `installers/gemini.py` | `~/.gemini/settings.json` |
| OpenCode | `opencode` | `installers/opencode.py` | `~/.opencode/plugin/repowire.ts` |

`repowire setup` auto-detects installed CLIs. Backend shows in `list_peers` TSV and peer context injection.

## Memory

Memory is stored in-repo at `.claude/memory/` (committed, shared across contributors).

**Sanitization rules — memory here is public:**
- Never store secrets, tokens, API keys, passwords, or auth credentials
- Never store absolute or home-relative paths — only paths relative to the repowire repo root (e.g. `repowire/daemon/app.py`, not `~/git/repowire/...` or `/Users/.../`)
- Never store IP addresses, internal hostnames, or private URLs
- Never store personal identifiers (emails, chat IDs, user IDs)
- Strip environment-specific details — keep memory portable across machines
- When in doubt, omit it. The memory should be safe to push to a public repo.

Use `.claude/memory/MEMORY.md` as the index. One file per memory, frontmatter format per the auto-memory system.

## Testing Notes

- Route tests: `httpx.AsyncClient` + `ASGITransport`, manually init deps
- WebSocket tests: `httpx-ws` + `ASGIWebSocketTransport`
- Hooks run from installed package - `uv tool install --force --reinstall .` after changes
- Mock `subprocess.Popen` in session handler tests to prevent ws-hook leaking to live daemon
- 231 tests covering routes, WebSocket, auth, query tracker, hooks, config, transcript

## Knowledge Graph

A graphify knowledge graph lives in `graphify-out/`. After significant code changes, run:

```bash
/graphify . --update   # incremental re-extraction (only changed files)
```

The graph covers 133 files, 1864 nodes, 5455 edges across the full codebase. Key god nodes: `AgentType`, `Config`, `PeerRegistry`, `MessageRouter`.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
