<div align="center">
  <picture>
    <source srcset="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-dark.webp" media="(prefers-color-scheme: dark)" width="150" height="150" />
    <img src="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-light.webp" alt="Repowire Logo" width="150" height="150" />
  </picture>

  <h1>Repowire</h1>
  <p>Mesh network for AI coding agents — enables Claude Code and OpenCode sessions to communicate.</p>

  [![PyPI](https://img.shields.io/pypi/v/repowire)](https://pypi.org/project/repowire/)
  [![CI](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml/badge.svg)](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml)
  [![Python](https://img.shields.io/pypi/pyversions/repowire)](https://pypi.org/project/repowire/)
  [![License](https://img.shields.io/pypi/l/repowire)](https://github.com/prassanna-ravishankar/repowire/blob/main/LICENSE)
</div>

## Why?

AI coding agents work great in a single repo, but multi-repo projects need a **context breakout** — a way to get information from other codebases. Most solutions are **async context breakouts**: memory banks, docs, persisted context. Repowire is a **sync context breakout**: live agents talking to each other about current code. Your `frontend` Claude can ask `backend` about API shapes and get a real answer from the actual codebase.

Read more: [the context breakout problem](https://prassanna.io/blog/vibe-bottleneck/) and [the idea behind Repowire](https://prassanna.io/blog/repowire/).

<details>
<summary><strong>How does repowire compare?</strong></summary>

| Project | Type | How it works | Best for |
|---------|------|--------------|----------|
| **Repowire** | Sync | Live agent-to-agent queries | Cross-repo collaboration, 5-10 peers |
| **[Gastown](https://github.com/steveyegge/gastown)** | Async | Work orchestration with persistent mail | Coordinated fleets, 20-30 agents |
| **[Claude Squad](https://github.com/smtg-ai/claude-squad)** | Isolated | Session management with worktrees | Multiple independent sessions |
| **[Memory Bank](https://docs.tinyfat.com/guides/memory-bank/)** | Async | Structured markdown files | Persistent project knowledge |

Repowire is a phone call (real-time, ephemeral). Gastown is email + project manager (async, persistent). For 5-10 agents, emergence works. For 20-30 grinding through backlogs, you probably need structure.

</details>


https://github.com/user-attachments/assets/e356ce7c-9454-4e41-93af-3991c6f391b9


## Installation

**Requirements:** macOS or Linux, Python 3.10+, tmux, [bun](https://bun.sh) (for channel transport)

```bash
uv tool install repowire
# or: pip install repowire
```

## Quick Start

```bash
# One-time setup — detects your Claude Code version and picks the best transport
repowire setup

# Verify everything is running
repowire status
```

Spawn two peers:

```bash
repowire peer new ~/projects/frontend --circle dev
repowire peer new ~/projects/backend --circle dev
```

The sessions auto-discover each other. In frontend's Claude:

```
"Ask backend what API endpoints they expose"
```

Claude uses the `ask_peer` tool, backend responds, and you get the answer back.

## How It Works

All peers connect to a central daemon via **WebSocket**. The daemon routes addressed messages between peers — no pub/sub, no topics. Messages go from peer A to peer B by name.

```
┌──────────────┐          ┌──────────────┐          ┌──────────────┐
│   Claude     │  channel │              │    WS    │   OpenCode   │
│   frontend   │◄────────►│    Daemon    │◄────────►│   api        │
└──────────────┘   (MCP)  │  :8377       │          └──────────────┘
                          │              │
┌──────────────┐  channel │              │
│   Claude     │◄────────►│              │
│   backend    │   (MCP)  └──────────────┘
└──────────────┘
```

**Message types:**
- `ask_peer` — request/response with correlation ID (blocks until answer, 300s timeout)
- `notify_peer` — fire-and-forget (no response expected)
- `broadcast` — fan-out to all peers in your circle

**Circles** are logical subnets (mapped to tmux sessions). Peers can only communicate within their circle unless explicitly bypassed.

### Claude Code Transport

On Claude Code v2.1.80+, repowire uses the native **channel transport** — an MCP server that delivers messages directly into Claude's context. Claude replies via a `reply` tool instead of transcript scraping. No tmux injection, no hooks for message delivery.

```
Claude Code ←stdio→ repowire-channel (MCP) ←WebSocket→ Daemon
```

- Messages arrive as `<channel source="repowire" from_peer="..." msg_type="...">` tags
- Queries include `correlation_id` — Claude calls the `reply` tool to respond
- Permission relay: approve tool use remotely from Telegram or the dashboard
- Requires claude.ai login (not available for API/Console key auth)

On older Claude Code versions, repowire falls back to **hooks + tmux injection** — the original transport that uses lifecycle hooks and `tmux send-keys` for message delivery.

<details>
<summary><strong>Legacy hooks transport</strong></summary>

For Claude Code < v2.1.80 or non-claude.ai auth:

- **SessionStart** — registers peer, spawns WebSocket hook, injects peer list as context
- **UserPromptSubmit** — marks peer BUSY
- **Stop** — extracts response from transcript, delivers query responses, posts chat turns for dashboard
- **Notification** (idle_prompt) — resets BUSY→ONLINE after interrupt

`repowire setup` auto-detects the version and installs the right transport.

</details>

<details>
<summary><strong>OpenCode integration</strong></summary>

OpenCode has a plugin SDK. The repowire plugin (`~/.opencode/plugin/repowire.ts`) maintains a persistent WebSocket connection and uses `client.session.prompt()` to inject queries.

</details>

## Control Plane

### Web Dashboard

<p align="center">
  <img src="images/repowire-hosted-2.png" alt="Peer grid overview" width="700" />
</p>

Monitor your agent mesh at `http://localhost:8377/dashboard`, or remotely via [repowire.io](https://repowire.io):

- **Peer overview** — online/busy/offline status, descriptions, project paths
- **Chat view** — conversation history per peer with tool call details
- **Compose bar** — send notifications or queries to any peer from the browser
- **Mobile responsive** — hamburger menu, touch-friendly compose

For remote access: `repowire setup --relay` connects your daemon to [repowire.io](https://repowire.io) via outbound WebSocket. Access your dashboard from any browser — no port forwarding, no VPN.

<details>
<summary>More screenshots</summary>
<br>
<p align="center">
  <img src="images/repowire-hosted-3.png" alt="Activity and message detail" width="700" />
</p>
<p align="center">
  <img src="images/repowire-hosted-1.png" alt="Chat view with relay" width="700" />
</p>
</details>

### Telegram Bot

Control your mesh from your phone. A Telegram bot registers as a peer — notifications from agents appear in your chat, messages you send get routed to peers.

```bash
TELEGRAM_BOT_TOKEN="..." TELEGRAM_CHAT_ID="..." repowire telegram start
```

- `/peers` — shows online peers with inline buttons
- Tap a peer → type your message → sent as notification
- Sticky routing: `/select repowire` → all messages go there until `/clear`
- Agents know `@telegram` is you — they can `notify_peer('telegram', ...)` to reach your phone

## MCP Tools

| Tool | Type | Description |
|------|------|-------------|
| `list_peers` | Query | List all peers with status, circle, path, description |
| `ask_peer` | Blocking | Send a question and wait for the response |
| `notify_peer` | Fire-and-forget | Send a notification — peer can `notify_peer` back when ready |
| `broadcast` | Fire-and-forget | Message all online peers in your circle |
| `whoami` | Query | Your own peer identity |
| `set_description` | Mutation | Update your task description, visible to all peers and the dashboard |
| `spawn_peer` | Mutation | Spawn a new agent session (requires [allowlist config](#configuration)) |
| `kill_peer` | Mutation | Kill a previously spawned session |

`list_peers` and `whoami` return TSV (more token-efficient than JSON). For long-running requests, prefer `notify_peer` over `ask_peer`.

## CLI Reference

```bash
repowire setup                    # Auto-detect transport, install everything
repowire setup --relay            # Same + enable remote dashboard via repowire.io
repowire status                   # Show what's installed and running
repowire serve                    # Run daemon in foreground
repowire serve --relay            # Run daemon with relay connection

repowire peer new PATH            # Spawn new peer in tmux
repowire peer new . --circle dev  # Spawn with custom circle
repowire peer list                # List peers and their status
repowire peer prune               # Remove offline peers

repowire telegram start           # Run Telegram bot (needs env vars)
repowire uninstall                # Remove all components
```

## Configuration

Config file: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "optional-secret"     # Require auth for WebSocket connections

  # Allow agents to spawn new sessions via MCP (both lists must be non-empty)
  spawn:
    allowed_commands:
      - claude
      - claude --dangerously-skip-permissions
    allowed_paths:
      - ~/git
      - ~/projects

relay:
  enabled: true                     # Connect to hosted relay
  url: "wss://repowire.io"
  api_key: "rw_..."                 # Auto-generated on first `repowire serve --relay`
```

Peers auto-register via WebSocket on session start — no manual config needed.

<details>
<summary><strong>Remote relay details</strong></summary>

```bash
repowire setup --relay
# ✓ Relay enabled
#   Dashboard: https://repowire.io/dashboard
```

Your daemon opens an outbound WebSocket to `repowire.io`. The relay bridges messages between daemons on different machines and proxies HTTP requests (dashboard, API) back through a cookie-authenticated tunnel.

```
Browser → repowire.io → enter key → cookie set → relay tunnels to local daemon
Daemon A ←WSS→ repowire.io ←WSS→ Daemon B (cross-machine mesh)
```

Self-host the relay: `repowire relay start --port 8000`

</details>

<details>
<summary><strong>Security</strong></summary>

- **WebSocket auth** — set `daemon.auth_token` in config to require bearer token for connections
- **CORS** — restricted to localhost origins (plus `repowire.io` when relay is enabled)
- **Spawn allowlist** — `daemon.spawn.allowed_commands` and `allowed_paths` must both be non-empty for MCP spawn to work
- **Channel gating** — channel transport requires claude.ai login; API/Console key users get hooks fallback

</details>

## License

MIT
