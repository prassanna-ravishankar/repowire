# Repowire

Local Service Mesh for AI Agents - Multi-repo orchestration for OpenCode.

## Overview

Repowire spawns multiple OpenCode sessions (one per repository) and links them via a local message bus. Agents can query each other, share state through a blackboard, and read files across repos—without human mediation.

## Installation

```bash
uv sync
```

## Usage

```bash
# Initialize config
repowire init

# Start mesh with TUI
repowire up

# Start headless (no TUI)
repowire up --no-tui

# Check status
repowire status
```

## Configuration

Create `repowire.yaml` in your workspace root:

```yaml
name: "My Project"

agents:
  backend:
    path: "./backend-api"
    model: "claude-sonnet-4-20250514"
    color: "blue"
    capabilities: ["api", "database"]
    description: "Backend API service"

  frontend:
    path: "./web-client"
    model: "claude-sonnet-4-20250514"
    color: "green"
    depends_on: ["backend"]
    description: "Frontend application"

settings:
  port_range_start: 3001
  blackboard_file: ".repowire/blackboard.json"
  git_branch_warnings: true
```

## Inter-Agent Tools

Each agent gets these MCP tools injected:

| Tool | Description |
|------|-------------|
| `list_peers()` | Discover available agents |
| `ask_peer(target, query)` | Query another agent and wait for response |
| `notify_peer(target, message)` | Send notification to an agent |
| `broadcast_update(message)` | Notify all agents |
| `read_peer_file(target, path)` | Read a file from another repo |
| `read_blackboard(key)` | Read shared state |
| `write_blackboard(key, value)` | Write shared state |

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Repowire Daemon                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
│  │  Message    │  │  Blackboard │  │   Process   │     │
│  │    Bus      │  │   (State)   │  │   Manager   │     │
│  └─────────────┘  └─────────────┘  └─────────────┘     │
└─────────────────────────────────────────────────────────┘
        │                  │                  │
        ▼                  ▼                  ▼
┌───────────────┐  ┌───────────────┐  ┌───────────────┐
│   Backend     │  │   Frontend    │  │     Infra     │
│   (OpenCode)  │  │   (OpenCode)  │  │   (OpenCode)  │
│   + MCP Tools │  │   + MCP Tools │  │   + MCP Tools │
└───────────────┘  └───────────────┘  └───────────────┘
```

## Requirements

- Python 3.10+
- OpenCode CLI installed and accessible
