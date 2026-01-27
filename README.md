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

**Requirements:** macOS or Linux, Python 3.10+, tmux

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

Spawn two peers:

```bash
# Using CLI
repowire peer new ~/projects/frontend --circle dev
repowire peer new ~/projects/backend --circle dev

# Or using TUI (press 'n' to spawn)
repowire top
```

The sessions auto-discover each other. In frontend's Claude:

```
"Ask backend what API endpoints they expose"
```

Claude uses the `ask_peer` tool, backend responds, and you get the answer back.

**What just happened?** See [How It Works](#how-it-works) for details.

## Dashboard

Monitor peer communication at `http://localhost:8377/dashboard` when the daemon is running.

- Real-time peer status (online/busy/offline)
- Communication event log (queries, responses, broadcasts)

## Terminal UI

Monitor and manage peers from the terminal:

```bash
repowire top
```

- Real-time peer status with vim-style navigation
- Spawn new peers with `n` key
- Attach to peer sessions with `s` key
- Event log with `e` key

## How It Works

### Workflow

Each coding agent runs in a **tmux window**. Windows are grouped into **tmux sessions** called **circles**. Peers can only communicate within their circle.

```
tmux session "dev" (circle)
├── window "frontend"  →  Claude Code session
├── window "backend"   →  Claude Code session
└── window "api"       →  OpenCode session
```

When you spawn a peer with `--circle dev`, repowire creates (or reuses) a tmux session named "dev" and adds a window for your agent.

### Backends

The **backend** determines which agent you're running and how messages are delivered:

| Backend | Agent | Message Delivery |
|---------|-------|------------------|
| **claudemux** | Claude Code | libtmux injection + hooks |
| **opencode** | OpenCode | WebSocket plugin + SDK |

### What's Installed

| Component | Location | Purpose |
|-----------|----------|---------|
| **Daemon** | System service (launchd/systemd) | Routes messages between peers, `127.0.0.1:8377` |
| **Hooks** | `~/.claude/settings.json` | Claude Code lifecycle events (claudemux) |
| **Plugin** | `~/.config/opencode/plugin/repowire.ts` | OpenCode integration (opencode) |
| **MCP Server** | Registered with Claude | `ask_peer`, `list_peers`, `notify_peer`, `broadcast` tools |
| **Config** | `~/.repowire/config.yaml` | Peer registry and settings |

<details>
<summary><strong>claudemux architecture</strong></summary>

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
┌─────────────────────────────────────────────────────────┐
│                      Daemon                             │
│  1. Receives query from frontend                        │
│  2. Injects query into backend's pane (libtmux)         │
│  3. Waits for Stop hook to send response                │
│  4. Returns response to frontend                        │
└─────────────────────────────────────────────────────────┘
```

**Why hooks?** Claude Code doesn't have an API. Hooks handle lifecycle:
- **SessionStart/End**: Register/unregister peer
- **UserPromptSubmit**: Mark peer busy
- **Stop**: Extract response from transcript
- **Notification** (idle_prompt): Reset after interrupt

</details>

<details>
<summary><strong>opencode architecture</strong></summary>

```
┌─────────────────────────────────────────────────────────────┐
│  OpenCode TUI                                               │
│  ┌────────────────────────────────────────────────────────┐ │
│  │  repowire plugin (TypeScript)                          │ │
│  │  • WebSocket connection to daemon                      │ │
│  │  • On query: client.session.prompt(sessionId, text)    │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                         ↕ WebSocket ws://127.0.0.1:8377/ws/plugin
┌─────────────────────────────────────────────────────────────┐
│                         Daemon                               │
│  • Routes queries to target plugin via WebSocket            │
│  • Correlation ID tracking for request/response matching    │
└─────────────────────────────────────────────────────────────┘
```

**Why WebSocket?** OpenCode has a plugin SDK. The plugin maintains a persistent connection for bidirectional communication.

</details>

Backend is auto-detected during `repowire setup` based on installed CLIs.

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

# TUI dashboard
repowire top                      # Launch terminal UI
repowire top --port 8080          # Custom daemon port

# Peer commands
repowire peer list                # List peers and their status
repowire peer ask NAME "query"    # Test: ask a peer a question
repowire peer new PATH            # Spawn new peer in tmux
repowire peer new . --circle dev  # Spawn with custom circle
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
