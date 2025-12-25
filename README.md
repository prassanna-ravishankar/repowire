# Repowire

Mesh network for Claude Code sessions - enables AI agents to communicate.

## Overview

Repowire creates a mesh where Claude Code sessions can communicate with each other. Frontend Claude can ask Backend Claude about API schemas. Infra Claude can notify everyone about deployments. Just like a human engineering team.

```
┌─────────────┐     "API schema?"     ┌─────────────┐
│  Frontend   │ ────────────────────► │   Backend   │
│   Claude    │ ◄──────────────────── │   Claude    │
│  (tmux)     │   "{id, name, ...}"   │   (tmux)    │
└─────────────┘                       └─────────────┘
```

## Installation

```bash
pip install repowire
# or
uv tool install repowire
```

## Quick Start

### 1. Install Claude Code hooks

```bash
repowire hooks install
```

### 2. Register your Claude sessions

Start Claude Code in tmux sessions:
```bash
tmux new -s frontend
cd ~/app/frontend && claude

# In another terminal
tmux new -s backend
cd ~/app/backend && claude
```

Register them as peers:
```bash
repowire peer register frontend --tmux-session frontend --path ~/app/frontend
repowire peer register backend --tmux-session backend --path ~/app/backend
```

### 3. Add MCP to Claude Code

Edit `~/.claude/settings.json`:
```json
{
  "mcpServers": {
    "repowire": {
      "command": "repowire",
      "args": ["mcp"]
    }
  }
}
```

### 4. Use in Claude

In your frontend Claude session:
```
Ask the backend peer what the API schema is for users
```

Claude will use the `ask_peer` tool to send a query to the backend session and return the response.

## MCP Tools

| Tool | Description |
|------|-------------|
| `list_peers()` | List all registered peers and their status |
| `ask_peer(peer_name, query)` | Ask a peer a question, wait for response |
| `notify_peer(peer_name, message)` | Send notification (fire-and-forget) |
| `broadcast(message)` | Send message to all peers |
| `register_peer(name, tmux_session, path)` | Register a new peer |

## CLI Commands

```bash
# Peer management
repowire peer list                          # List peers and status
repowire peer register NAME -t TMUX -p PATH # Register a peer
repowire peer unregister NAME               # Remove a peer
repowire peer ask NAME "query"              # Test: ask a peer

# Hook management
repowire hooks install                      # Install Claude Code hooks
repowire hooks uninstall                    # Remove hooks
repowire hooks status                       # Check installation

# Daemon (for relay mode)
repowire daemon start --relay-url URL       # Start daemon

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
repowire daemon start \
  --relay-url wss://relay.repowire.io \
  --api-key rw_xxx
```

## Configuration

Config file: `~/.repowire/config.yaml`

```yaml
relay:
  enabled: false
  url: "wss://relay.repowire.io"
  api_key: null

peers:
  frontend:
    tmux_session: "frontend"
    path: "/Users/you/app/frontend"
  backend:
    tmux_session: "backend"
    path: "/Users/you/app/backend"

daemon:
  auto_reconnect: true
  heartbeat_interval: 30
```

## How It Works

1. **Local mesh**: Claude sessions in tmux communicate directly via libtmux
2. **Hook integration**: Claude Code's Stop hook notifies Repowire when responses complete
3. **Relay server**: For multi-machine setups, a WebSocket relay routes messages
4. **MCP tools**: Claude accesses the mesh via MCP tools

## Requirements

- Python 3.10+
- tmux
- Claude Code with hooks support

## License

MIT
