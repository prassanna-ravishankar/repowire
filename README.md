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
repowire peer new ~/projects/frontend --circle dev
repowire peer new ~/projects/backend --circle dev
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
- Communication event log with query/response matching

## How It Works

### Architecture

All peers connect to a central daemon via **WebSocket**. The daemon routes messages between peers — it doesn't care what agent type a peer runs. Claude Code and OpenCode peers are treated identically.

```
┌──────────────┐          ┌──────────────┐          ┌──────────────┐
│   Claude     │    WS    │              │    WS    │   OpenCode   │
│   frontend   │◄────────►│    Daemon    │◄────────►│   api        │
└──────────────┘          │  :8377       │          └──────────────┘
                          │              │
┌──────────────┐    WS    │              │
│   Claude     │◄────────►│              │
│   backend    │          └──────────────┘
└──────────────┘
```

### Circles

Peers are grouped into **circles** (tmux sessions). Peers can only communicate within their circle.

```
tmux session "dev" (circle)
├── window "frontend"  →  Claude Code session
├── window "backend"   →  Claude Code session
└── window "api"       →  OpenCode session
```

When you spawn a peer with `--circle dev`, repowire creates (or reuses) a tmux session named "dev" and adds a window for your agent.

### Agent Types

The **agent type** identifies which AI coding tool a peer runs. The daemon treats all types identically — the difference is only in how the agent integrates with repowire.

| Agent Type | Tool | Integration |
|------------|------|-------------|
| **claude-code** | Claude Code | Hooks + MCP server + WebSocket hook |
| **opencode** | OpenCode | TypeScript plugin via SDK |

Agent type is auto-detected during `repowire setup` based on installed CLIs.

### What's Installed

| Component | Location | Purpose |
|-----------|----------|---------|
| **Daemon** | System service (launchd/systemd) | Routes messages between peers, `127.0.0.1:8377` |
| **Hooks** | `~/.claude/settings.json` | Claude Code lifecycle events |
| **Plugin** | `~/.opencode/plugin/repowire.ts` | OpenCode integration |
| **MCP Server** | Registered with Claude | `ask_peer`, `list_peers`, `notify_peer`, `broadcast`, `whoami` tools |
| **Config** | `~/.repowire/config.yaml` | Peer registry and settings |

### Message Flow

```
1. Claude calls ask_peer("backend", "What endpoints exist?")
2. MCP server → HTTP POST /query → Daemon
3. Daemon routes query via WebSocket to backend's hook/plugin
4. Backend agent processes, responds
5. Response flows back via WebSocket → Daemon → MCP → Claude
```

<details>
<summary><strong>Claude Code integration details</strong></summary>

Claude Code doesn't have an API. Repowire uses **hooks** for lifecycle management and a **persistent WebSocket process** for message delivery:

- **SessionStart** → Generates a stable unique peer name from the first 8 characters of Claude's `session_id` (same name across resumes, new name for fresh invocations). Spawns a `websocket_hook.py` background process that maintains a WebSocket connection to the daemon. Outputs peer list as context for Claude.
- **UserPromptSubmit** → Marks peer as BUSY
- **Stop** → Extracts response from transcript, writes to file. The WebSocket hook picks it up and forwards via WebSocket.
- **SessionEnd** → No-op (fires spuriously during agentic loops; the WebSocket hook self-terminates via pane liveness checking ~30s after agent exits)
- **Notification** (idle_prompt) → Resets BUSY→ONLINE after interrupt

**Peer State Machine:** `OFFLINE → ONLINE ↔ BUSY`

</details>

<details>
<summary><strong>OpenCode integration details</strong></summary>

OpenCode has a plugin SDK. The repowire plugin (`~/.opencode/plugin/repowire.ts`) maintains a persistent WebSocket connection to the daemon and uses `client.session.prompt()` to inject queries into the active session.

</details>

## CLI Reference

```bash
# Main commands
repowire setup                    # Install hooks, MCP server, daemon service
repowire setup --no-service       # Skip daemon service (run manually with 'serve')
repowire status                   # Show what's installed and running
repowire uninstall                # Remove all components

# Daemon
repowire serve                    # Run daemon in foreground
repowire build-ui                 # Build web dashboard (development)

# Peer management
repowire peer list                # List peers and their status
repowire peer new PATH            # Spawn new peer in tmux
repowire peer new . --circle dev  # Spawn with custom circle
repowire peer ask NAME "query"    # Send a query (testing utility)
repowire peer prune               # Remove offline peers
```

<details>
<summary>Advanced commands (hidden from <code>--help</code>)</summary>

```bash
# Agent-specific
repowire claude status            # Check hooks installation
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

### Security

**WebSocket Authentication (Optional)**

To require authentication for WebSocket connections, add `auth_token` to your config:

```yaml
# ~/.repowire/config.yaml
daemon:
  auth_token: "your-secret-token-here"
```

For OpenCode peers, set the environment variable:
```bash
export REPOWIRE_AUTH_TOKEN="your-secret-token-here"
```

The daemon also restricts CORS to localhost origins only.

### Multi-Machine Relay

> **Experimental** - Not production ready. `relay.repowire.io` is not yet available.

For agents on different machines:

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
  auth_token: "optional-secret"  # Optional: require auth for WebSocket connections

relay:
  enabled: false
  url: "wss://relay.repowire.io"
  api_key: null

# Peers auto-register via WebSocket on session start
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
