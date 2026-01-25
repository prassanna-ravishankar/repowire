<div align="center">
  <picture>
    <source srcset="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-dark.webp" media="(prefers-color-scheme: dark)" width="150" height="150" />
    <img src="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-light.webp" alt="Repowire Logo" width="150" height="150" />
  </picture>

  <h1>Repowire</h1>
  <p>Mesh network for AI coding agents - enables Claude Code and OpenCode sessions to communicate.</p>

  [![PyPI](https://img.shields.io/pypi/v/repowire)](https://pypi.org/project/repowire/)
  [![CI](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml/badge.svg)](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml)
  [![Python](https://img.shields.io/pypi/pyversions/repowire)](https://pypi.org/project/repowire/)
  [![License](https://img.shields.io/pypi/l/repowire)](https://github.com/prassanna-ravishankar/repowire/blob/main/LICENSE)
</div>

## Why?

AI coding agents work great in a single repo, but multi-repo projects need a **context breakout** - a way to get information from other codebases. Most solutions are **async context breakouts**: memory banks, docs, persisted context. Repowire is a **sync context breakout**: live agents talking to each other about current code. Your `frontend` Claude can ask `backend` about API shapes and get a real answer from the actual codebase.

Read more about it in my blog where I describe [the context breakout problem](https://prassanna.io/blog/vibe-bottleneck/) and [the idea behind building Repowire](https://prassanna.io/blog/repowire/).

<details>
<summary><strong>How does repowire compare with other projects?</strong></summary>

| Project | Type | How it works | Best for |
|---------|------|--------------|----------|
| **Repowire** | Sync | Live agent-to-agent queries | Cross-repo collaboration, 5-10 peers |
| **[Gastown](https://github.com/steveyegge/gastown)** | Async | Work orchestration with persistent mail (beads) | Coordinated fleets, 20-30 agents, single codebase |
| **[Claude Squad](https://github.com/smtg-ai/claude-squad)** | Isolated | Session management with worktrees | Multiple independent Claude sessions |
| **[Memory Bank](https://docs.tinyfat.com/guides/memory-bank/)** | Async | Structured markdown files, human-curated | Persistent project knowledge |
| **[Polyrepo MCP](https://blackdoglabs.io/blog/claude-code-decoded-multi-repo-context)** | Async | MCP server loading cross-repo context | Pre-loaded multi-repo context |

**Repowire vs Gastown:** Repowire is a phone call (real-time, ephemeral). Gastown is email + project manager (async, persistent, orchestrated). Repowire bets on emergence - give agents a way to talk and a working methodology develops organically as sessions discover how to best use each other. Gastown bets on structure - Mayor, convoys, formulas. For 5-10 agents, emergence works. For 20-30 grinding through backlogs, you probably need the structure.

</details>

[![asciicast](https://asciinema.org/a/772201.svg)](https://asciinema.org/a/772201)

## Installation

**Requirements:** macOS or Linux, Python 3.10+, tmux (for claudemux backend)

```bash
# Install from PyPI
uv tool install repowire
# or
pip install repowire
```

## Quick Start

```bash
# One-time setup - installs hooks, MCP server, and daemon service
repowire setup

# Verify everything is running
repowire status
```

Now start two Claude sessions in tmux:

```bash
# Terminal 1
tmux new-session -s dev -n frontend
cd ~/projects/frontend && claude

# Terminal 2 (or tmux split)
tmux new-window -t dev -n backend
cd ~/projects/backend && claude
```

The sessions auto-discover each other. In frontend's Claude:

```
"Ask backend what API endpoints they expose"
```

Claude uses the `ask_peer` tool, backend responds, and you get the answer back.

**What just happened?** See [How It Works: claudemux](#claudemux-default) for details.

## Dashboard

Monitor peer communication at `http://localhost:8377/dashboard` when the daemon is running.

- Real-time peer status (online/busy/offline)
- Communication event log (queries, responses, broadcasts)

## How It Works

### claudemux (default)

For Claude Code sessions running in tmux. This is the tested, production-ready backend.

#### What's Installed

| Component | Location | Purpose |
|-----------|----------|---------|
| **Daemon** | System service (launchd/systemd) | Routes messages between peers, runs on `127.0.0.1:8377` |
| **Hooks** | `~/.claude/settings.json` | SessionStart/End (register/cleanup), UserPromptSubmit (busy), Stop (response), Notification (idle recovery) |
| **MCP Server** | Registered with Claude | Provides `ask_peer`, `list_peers`, `notify_peer`, `broadcast` tools |
| **Config** | `~/.repowire/config.yaml` | Peer registry and settings |
| **Logs** | `~/.repowire/daemon.log` | Daemon output |

<details>
<summary><strong>Architecture</strong></summary>

```
┌─────────────┐                           ┌─────────────┐
│  frontend   │    ask_peer("backend")    │   backend   │
│   Claude    │ ─────────────────────────►│   Claude    │
│             │                           │             │
│             │ ◄─────────────────────────│             │
│             │      response text        │             │
└─────────────┘                           └─────────────┘
       │                                         │
       │ MCP tool call                           │ Stop hook captures
       ▼                                         ▼ response from transcript
┌─────────────────────────────────────────────────────┐
│                      Daemon                          │
│                   127.0.0.1:8377                     │
│                                                     │
│  1. Receives query from frontend                    │
│  2. Looks up backend's tmux session                 │
│  3. Injects query into backend's pane (libtmux)    │
│  4. Waits for Stop hook to send response            │
│  5. Returns response to frontend                    │
└─────────────────────────────────────────────────────┘
```

**Why tmux?** Claude Code runs in a terminal. Tmux gives us programmatic access to inject queries (via `send_keys`) into another session's pane.

**Why hooks?** Claude Code doesn't have an API. Hooks are the only extension point:
- **SessionStart**: Registers peer with daemon, injects list of available peers into Claude's context
- **SessionEnd**: Marks peer offline, cancels pending queries
- **UserPromptSubmit**: Marks peer as busy while processing
- **Stop**: Reads transcript, extracts Claude's response, sends to daemon
- **Notification** (idle_prompt): Resets peer to online after interrupt (Stop doesn't fire on Escape)

**Why a central daemon?** Single source of truth for peer registry. Runs as a system service so it survives reboots and is always available when Claude sessions start.

</details>

---

### opencode

For OpenCode sessions. Uses a WebSocket plugin that connects to the daemon and injects queries via OpenCode's SDK.

#### What's Installed

| Component | Location | Purpose |
|-----------|----------|---------|
| **Daemon** | System service | Routes messages via WebSocket, `127.0.0.1:8377` |
| **Plugin** | `~/.config/opencode/plugin/repowire.ts` | WebSocket client, provides `ask_peer`, `list_peers`, `notify_peer`, `broadcast`, `whoami` tools |
| **Config** | `~/.repowire/config.yaml` | Peer registry |

<details>
<summary><strong>Architecture</strong></summary>

```
┌─────────────────────────────────────────────────────────────┐
│  OpenCode TUI - User's session                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │  repowire plugin (TypeScript)                          │ │
│  │  • WebSocket connection to daemon                      │ │
│  │  • Tracks activeSessionId via event hooks              │ │
│  │  • On query: client.session.prompt(sessionId, text)    │ │
│  │  • Injects mesh context into system prompt             │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                         ↕ WebSocket ws://127.0.0.1:8377/ws/plugin
┌─────────────────────────────────────────────────────────────┐
│                         Daemon                               │
│  • WebSocket endpoint for plugin connections                │
│  • Routes queries to target plugin via WebSocket            │
│  • Correlation ID tracking for request/response matching    │
└─────────────────────────────────────────────────────────────┘
```

**How it works:**
1. Plugin connects to daemon via WebSocket on startup
2. Plugin registers peer (name = folder name) and tracks session ID
3. When query arrives, plugin calls `session.prompt()` to inject into user's chat
4. Response extracted and sent back via WebSocket
5. System prompt automatically includes list of online peers

**Why WebSocket?** OpenCode plugins run inside the TUI process. WebSocket provides persistent bidirectional communication without needing external HTTP servers.

</details>

OpenCode support is auto-detected during `repowire setup` when the `opencode` CLI is installed.

## CLI Reference

```bash
# Main commands
repowire setup                    # Install hooks, MCP server, daemon service
repowire setup --no-service       # Skip daemon service (run manually with 'serve')
repowire status                   # Show what's installed and running
repowire uninstall                # Remove all components

# Manual daemon control
repowire serve                    # Run daemon in foreground
repowire build-ui                 # Build dashboard (for development)

# Peer commands
repowire peer list                # List peers and their status
repowire peer ask NAME "query"    # Test: ask a peer a question
```

<details>
<summary>Advanced commands (hidden from <code>--help</code>)</summary>

```bash
# Backend-specific
repowire claudemux status         # Check hooks installation
repowire opencode status          # Check plugin installation

# Service management
repowire service install          # Install daemon as system service
repowire service uninstall        # Remove system service
repowire service status           # Check service status

# Config
repowire config show              # Show current configuration
repowire config path              # Show config file path
```

</details>

## Advanced

### Multi-Machine Relay

> ⚠️ **Experimental** - Not production ready. `relay.repowire.io` is not yet available.

For Claude sessions on different machines:

```bash
# Self-host a relay server
repowire relay start --port 8000
repowire relay generate-key --user-id myuser

# On each machine, configure relay in ~/.repowire/config.yaml:
# relay:
#   enabled: true
#   url: "wss://your-relay-server:8000"
#   api_key: "your-key"
```

### Configuration Reference

Config file: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  # Per-peer routing auto-detects backend based on peer config

relay:
  enabled: false
  url: "wss://relay.repowire.io"
  api_key: null

# Peers auto-populate via SessionStart hook
peers:
  frontend:
    name: frontend
    path: "/path/to/frontend"
    tmux_session: "dev:frontend"
    metadata:
      branch: "main"  # auto-detected from git
```

## License

MIT
