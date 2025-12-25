# Repowire

Happy++ MCP - Enables Claude sessions to communicate via Happy Cloud.

## Overview

Repowire is a simplified MCP server that connects your Claude Code session to other Happy CLI sessions. Query other sessions, spawn new ones, and send messages across sessions—all through Happy Cloud's existing transport.

## Installation

```bash
uv tool install git+https://github.com/prassanna-ravishankar/repowire.git
```

## Setup

1. **Authenticate with Happy**

   You need your Happy **backup secret key** (the one Happy asked you to save when you signed up):

   ```bash
   repowire auth happy --secret "XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-..."
   ```

2. **Add to Claude Code's MCP config**

   Edit `~/.config/claude-code/mcp_config.json`:
   ```json
   {
     "mcpServers": {
       "repowire": {
         "command": "repowire",
         "args": ["mesh"]
       }
     }
   }
   ```

3. **Restart Claude Code**

## Tools

Three tools for inter-session communication:

| Tool | Description |
|------|-------------|
| `list_sessions()` | List all Happy CLI sessions |
| `send_message(session_id, text, permission_mode?)` | Send message to a session, wait for response |
| `create_session(path)` | Spawn new Happy CLI session at path |

## Usage Examples

**List available sessions:**
```python
list_sessions()
# Returns: [{"id": "abc123", "path": "/path/to/repo", ...}, ...]
```

**Send a message:**
```python
send_message(
    session_id="abc123",
    text="What tests are failing?",
    permission_mode="yolo"  # or "default", "plan", etc.
)
```

**Spawn a new session:**
```python
create_session(path="/Users/me/projects/frontend")
```

## Requirements

- Python 3.10+
- Happy CLI installed and authenticated
