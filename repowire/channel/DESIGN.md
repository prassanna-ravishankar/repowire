# Repowire Channel Transport — Design Doc

## Overview

A Claude Code Channel MCP server that replaces hooks + tmux injection with native message delivery. One TypeScript file (~200 lines) replaces 5 hook handlers + websocket_hook.py.

## What it replaces

| Current (hooks+tmux) | Channel equivalent |
|---|---|
| websocket_hook.py (background daemon, tmux send-keys) | Channel MCP server (Claude Code subprocess, native delivery) |
| session_handler.py (registration, context injection) | Channel init (register peer, return context via instructions) |
| stop_handler.py (transcript scraping, response delivery) | Reply tool (Claude calls it directly) |
| prompt_handler.py (mark BUSY) | Channel status message on user prompt |
| notification_handler.py (idle→ONLINE) | Channel status message on idle |
| installers/claude_code.py (settings.json hooks) | .mcp.json channel registration |
| pending-{pane}.json (flock-protected CID file) | In-memory correlation map |

## What it keeps

- **Stop hook for chat_turns** — Channel doesn't see Claude's full conversation (user/assistant text + tool calls). Keep a minimal stop hook that only posts to `/events/chat` for dashboard visualization. No response delivery, no correlation IDs.
- **Daemon unchanged** — Channel connects via same WebSocket `/ws` endpoint.

## Architecture

```
Claude Code ←stdio→ repowire-channel.ts ←WebSocket→ Daemon ←WS→ Other peers
                    (MCP server)
```

## Channel server responsibilities

### 1. Registration (replaces session_handler.py)

On startup:
- Derive `display_name` from `CLAUDE_SESSION_ID` env var (first 8 chars)
- Connect to daemon `/ws` with `{type: "connect", display_name, circle, backend, path}`
- Fetch peer list via HTTP `GET /peers`

Via `instructions` field:
- Tell Claude about available peers
- Explain `@dashboard` and `@telegram` are human
- Instruct Claude to call `set_description()` early

### 2. Message delivery (replaces websocket_hook.py)

Daemon → Channel → Claude:
- Receive `query`/`notify`/`broadcast` via WS
- Emit `notifications/claude/channel` with `{content: text, meta: {from_peer, type, correlation_id}}`
- Claude sees: `<channel source="repowire" from_peer="frontend" type="query" correlation_id="abc">What endpoints?</channel>`

### 3. Reply tool (replaces stop_handler.py response delivery)

Claude → Channel → Daemon:
- Expose `reply` tool: `{correlation_id, text}`
- On call: send WS message `{type: "response", correlation_id, text}` to daemon
- Daemon resolves the pending query future

### 4. Status tracking (replaces prompt/notification handlers)

- Claude Code lifecycle events flow through the channel
- On query/notify received while processing → peer is BUSY (daemon already knows from WS activity)
- Actually: the daemon's existing state machine (WS connect→ONLINE, query in-flight→BUSY via MCP tools) may be sufficient

### 5. Permission relay (bonus!)

- Declare `claude/channel/permission` capability
- Forward tool approval prompts to Telegram bot
- User approves/denies from phone
- Major UX win: no more terminal-only permission dialogs

## File structure

```
repowire/channel/
├── server.ts        — the channel MCP server (~200 lines)
├── package.json     — deps: @modelcontextprotocol/sdk, ws
└── .mcp.json        — channel registration template
```

## Setup flow

```bash
repowire setup  # detects Claude Code v2.1.80+
# → copies .mcp.json to project or ~/.claude.json
# → configures daemon URL
# → falls back to hooks for older Claude Code
```

## Migration path

1. **Phase 1**: Channel coexists with hooks. `repowire setup` detects version and picks one.
2. **Phase 2**: Channel becomes default. Hooks stay as legacy fallback.
3. **Phase 3**: Hooks deprecated. Channel-only.

## Validated (2026-03-24)

Tested on Claude Code v2.1.81 with `--dangerously-load-development-channels server:repowire-channel`:

- ✅ Channel server connects to daemon via WebSocket and registers as peer
- ✅ `notify_peer` → arrives as `← repowire-channel:` message in Claude's context
- ✅ `ask_peer` → arrives with correlation_id, Claude calls `reply` tool with correct correlation_id
- ✅ Full request/response loop works (ask → reply → future resolved)
- ✅ Claude can use existing MCP tools (notify_peer, set_description) alongside channel
- ✅ Permission relay capability declared (not yet tested end-to-end)

Display name defaults to "channel" when `CLAUDE_SESSION_ID` not available as env var. Needs investigation — may need to read from Claude's MCP init message or use a different env var.

## Open questions

- How to get `session_id` in the channel server? (not available as env var in current test)
- Keep minimal stop hook for chat_turn dashboard events? Or find another way?
- `.mcp.json` placement: project root vs `~/.claude.json` for global install?
- Package as a Claude Code plugin for easy distribution?
