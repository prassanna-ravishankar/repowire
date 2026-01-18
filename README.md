# Repowire

Mesh network for AI coding agents (Claude Code, OpenCode) - enables sessions to communicate.

## Quick Start

```bash
# One-time setup (installs hooks + MCP server)
repowire setup --dev  # use --dev for local development

# Start daemon
repowire serve

# Start Claude in tmux windows - peers auto-register via SessionStart hook
tmux new-window -n alice
cd ~/projects/frontend && claude

tmux new-window -n bob
cd ~/projects/backend && claude
```

Alice and Bob can now talk:
```
# In Alice's Claude session:
"Ask bob what API endpoints they have"
```

## How It Works

```
┌─────────────┐                        ┌─────────────┐
│   Alice     │  ask_peer("bob", ...)  │    Bob      │
│  (claude)   │ ───────────────────►   │  (claude)   │
│             │                        │             │
│             │  ◄─────────────────    │             │
│             │   Stop hook captures   │             │
└─────────────┘   response & returns   └─────────────┘
        │                                     │
        └──────────┐           ┌──────────────┘
                   ▼           ▼
              ┌─────────────────────┐
              │   HTTP Daemon       │
              │  127.0.0.1:8377     │
              │                     │
              │  - routes queries   │
              │  - tracks pending   │
              │  - receives hooks   │
              └─────────────────────┘
```

1. **SessionStart hook** registers peer with metadata (name = folder name, git branch)
2. **SessionStart hook** injects peer context into Claude (lists available peers)
3. **ask_peer** sends query to daemon, daemon injects into target's tmux pane
4. **Target Claude** responds naturally
5. **Stop hook** fires at end of turn, captures response from transcript
6. **Response** routes back to caller via daemon

## MCP Tools

| Tool | Description |
|------|-------------|
| `list_peers()` | List all registered peers and their status |
| `ask_peer(peer_name, query)` | Ask a peer a question, wait for response |
| `notify_peer(peer_name, message)` | Proactively share info (don't use for responses) |
| `broadcast(message)` | Send message to all peers (announcements only) |

Note: Peers auto-register via SessionStart hook. Your response to `ask_peer` queries is captured automatically - don't use `notify_peer` to respond.

## CLI Commands

```bash
# Peer management
repowire peer list                          # List peers and status
repowire peer register NAME -t TMUX -p PATH # Register a peer (claudemux)
repowire peer register NAME -u URL -p PATH  # Register a peer (opencode)
repowire peer unregister NAME               # Remove a peer
repowire peer ask NAME "query"              # Test: ask a peer

# Backend-specific hook/plugin management
repowire claudemux install                  # Install Claude Code hooks
repowire claudemux uninstall                # Remove hooks
repowire claudemux status                   # Check installation
repowire opencode install                   # Install OpenCode plugin
repowire opencode status                    # Check installation

# Daemon
repowire serve                              # Start daemon (default backend)
repowire serve --backend claudemux          # Start with specific backend
repowire serve --backend opencode           # Start with OpenCode backend

# Relay server (self-hosted)
repowire relay start --port 8000            # Start relay server
repowire relay generate-key                 # Generate API key

# Configuration
repowire config show                        # Show current config
repowire config path                        # Show config file path
```

## Multi-Machine Setup

For Claude sessions on different machines, use the relay server:

### 1. Deploy relay (or use repowire.io)

```bash
# Self-hosted
repowire relay start --port 8000

# Or use hosted relay at relay.repowire.io
```

### 2. Generate API key

```bash
repowire relay generate-key --user-id myuser
# Save the generated key
```

### 3. Start daemon on each machine

```bash
# Configure relay in ~/.repowire/config.yaml, then:
repowire serve
```

## Configuration

Config file: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  backend: "claudemux"  # or "opencode"
  auto_reconnect: true
  heartbeat_interval: 30

relay:
  enabled: false
  url: "wss://relay.repowire.io"
  api_key: null

# Peers auto-populate via SessionStart hook
peers:
  frontend:
    name: frontend
    path: "/Users/you/app/frontend"
    tmux_session: "0:frontend"    # claudemux backend
    session_id: "abc123..."       # set by hook
    metadata:
      branch: "feat/new-ui"       # git branch, auto-detected
  backend:
    name: backend
    path: "/Users/you/app/backend"
    opencode_url: "http://localhost:4096"  # opencode backend
    session_id: "def456..."
    metadata:
      branch: "main"
```

## Testing the Flow

Use tmux MCP to set up test peers:

1. Start daemon: `repowire serve &`
2. Create windows for alice and bob via `tmux-mcp create-window`
3. In each window, run: `cd ~/development/projects/<some-project> && claude`
4. Verify with `repowire peer list` - peers show as folder names (e.g., `a2a-chat`)
5. In alice's session: "Ask a2a-chat what this project does"
6. Clean up: kill the tmux windows via `tmux-mcp kill-window`

Note: Peer name = folder name, not tmux window name.

## Requirements

- Python 3.10+
- tmux (for claudemux backend)
- Claude Code with hooks support (for claudemux backend)
- OpenCode (for opencode backend)

## License

MIT
