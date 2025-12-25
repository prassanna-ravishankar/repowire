# Repowire - Plan

## Vision

**Repowire enables Claude Code sessions to communicate with each other.**

Modern development involves multiple contexts - frontend, backend, infrastructure, documentation. Today, each Claude Code session is isolated. Repowire creates a mesh where coding agents can collaborate, delegate, ask questions, and share context - just like a human engineering team.

```
┌─────────────┐     "What's the API      ┌─────────────┐
│  Frontend   │     contract for users?" │   Backend   │
│   Claude    │ ───────────────────────► │   Claude    │
│             │ ◄─────────────────────── │             │
│  /app/web   │    "{id, name, email}"   │  /app/api   │
└─────────────┘                          └─────────────┘
       │                                        │
       │    "Deploy preview ready"              │
       └────────────────────────────────────────┘
                        │
                        ▼
               ┌─────────────┐
               │    Infra    │
               │   Claude    │
               │  /app/infra │
               └─────────────┘
```

## Core Concepts

### Peers
A **peer** is a named coding session. It has:
- **Name**: Human-readable identifier (e.g., "frontend", "backend", "api-service")
- **Path**: Working directory (e.g., `/Users/prass/app/frontend`)
- **Machine**: Which computer it's running on
- **Status**: Online, busy, offline

### Messages
Peers communicate via messages:
- **Query**: Ask a question, wait for response
- **Notification**: Fire-and-forget message
- **Broadcast**: Message to all peers

### The Mesh
The mesh is the network of all connected peers. Repowire provides:
- Discovery: Find other peers
- Routing: Send messages to the right peer
- Presence: Know who's online

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Repowire Relay (repowire.io)                 │
│                                                                 │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐             │
│  │   Session   │  │   Message   │  │    Auth     │             │
│  │  Registry   │  │   Router    │  │   Service   │             │
│  └─────────────┘  └─────────────┘  └─────────────┘             │
│         │                │                │                     │
│         └────────────────┴────────────────┘                     │
│                          │                                      │
│                    WebSocket Hub                                │
└─────────────────────────────────────────────────────────────────┘
                           │
          ┌────────────────┼────────────────┐
          │                │                │
          ▼                ▼                ▼
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│   Machine A  │  │   Machine B  │  │   Machine C  │
│              │  │              │  │              │
│ ┌──────────┐ │  │ ┌──────────┐ │  │ ┌──────────┐ │
│ │ Repowire │ │  │ │ Repowire │ │  │ │ Repowire │ │
│ │  Client  │ │  │ │  Client  │ │  │ │  Client  │ │
│ └──────────┘ │  │ └──────────┘ │  │ └──────────┘ │
│      │       │  │      │       │  │      │       │
│ ┌────┴────┐  │  │ ┌────┴────┐  │  │ ┌────┴────┐  │
│ │  tmux   │  │  │ │  tmux   │  │  │ │  tmux   │  │
│ │sessions │  │  │ │sessions │  │  │ │sessions │  │
│ └─────────┘  │  │ └─────────┘  │  │ └─────────┘  │
└──────────────┘  └──────────────┘  └──────────────┘
```

### Component 1: Relay Server

The relay is the central coordination point. It:

1. **Maintains Session Registry**
   - Which peers are online
   - Their names, paths, machines
   - Last seen timestamps

2. **Routes Messages**
   - Receives message from sender
   - Finds target peer(s)
   - Forwards to appropriate client daemon

3. **Handles Authentication**
   - API keys per user
   - Session tokens
   - Rate limiting

**Tech Stack:**
- Python (FastAPI + python-socketio)
- Redis for pub/sub and session state
- PostgreSQL for persistence (optional)

**API:**
```
WebSocket Events:
  → register(name, path, machine)     # Announce a session
  → unregister(name)                  # Session going offline
  → list_peers()                      # Get all online peers
  → send_message(target, payload)     # Send to specific peer
  → broadcast(payload)                # Send to all peers

  ← peer_online(name, path, machine)  # Peer came online
  ← peer_offline(name)                # Peer went offline
  ← message(from, payload)            # Incoming message
```

### Component 2: Client Daemon

The client runs on each machine. It:

1. **Manages Local Sessions**
   - Discovers tmux sessions running Claude
   - Registers them with the relay
   - Tracks session state

2. **Bridges Relay ↔ Local**
   - Receives messages from relay
   - Injects into appropriate tmux session
   - Captures responses
   - Sends back to relay

3. **Provides Session Abstraction**
   - Uses libtmux for tmux interaction
   - Handles output capture and parsing
   - Detects when Claude is "done" responding

**Tech Stack:**
- Python
- libtmux for tmux interaction
- websockets/socketio client
- Runs as daemon or foreground process

**Commands:**
```bash
repowire daemon start              # Start client daemon
repowire daemon stop               # Stop daemon
repowire daemon status             # Show status

repowire session list              # List local Claude sessions
repowire session register <name>   # Register a tmux session as peer

repowire peer list                 # List all peers in mesh
repowire peer ask <name> <query>   # Ask a peer (CLI test)
```

### Component 3: MCP Server

The MCP server gives Claude access to the mesh. It:

1. **Exposes Tools**
   ```python
   @mcp.tool()
   def list_peers() -> list[Peer]:
       """List all peers in the mesh."""

   @mcp.tool()
   def ask_peer(name: str, query: str, timeout: int = 120) -> str:
       """Ask a peer a question and wait for response."""

   @mcp.tool()
   def notify_peer(name: str, message: str) -> None:
       """Send a notification to a peer (fire-and-forget)."""

   @mcp.tool()
   def broadcast(message: str) -> None:
       """Send a message to all peers."""

   @mcp.tool()
   def read_peer_file(name: str, file_path: str) -> str:
       """Read a file from a peer's working directory."""

   @mcp.tool()
   def write_shared_state(key: str, value: str) -> None:
       """Write to shared state that all peers can read."""

   @mcp.tool()
   def read_shared_state(key: str = None) -> dict:
       """Read from shared state."""
   ```

2. **Communicates with Client Daemon**
   - Via local Unix socket or HTTP
   - Daemon handles actual relay communication

**MCP Config:**
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

## Message Flow

### Query Flow (ask_peer)

```
Claude A                  Client A        Relay         Client B                  Claude B
   │                         │              │              │                         │
   │ ask_peer("backend",     │              │              │                         │
   │   "API schema?")        │              │              │                         │
   │────────────────────────►│              │              │                         │
   │                         │  send_msg    │              │                         │
   │                         │─────────────►│              │                         │
   │                         │              │  forward_msg │                         │
   │                         │              │─────────────►│                         │
   │                         │              │              │  tmux send-keys         │
   │                         │              │              │  "API schema?"          │
   │                         │              │              │────────────────────────►│
   │                         │              │              │                         │
   │                         │              │              │  (Claude processes)     │
   │                         │              │              │                         │
   │                         │              │              │  tmux capture-pane      │
   │                         │              │              │◄────────────────────────│
   │                         │              │  response    │                         │
   │                         │              │◄─────────────│                         │
   │                         │  response    │              │                         │
   │◄────────────────────────│◄─────────────│              │                         │
   │                         │              │              │                         │
   │ "{id, name, email}"     │              │              │                         │
   │                         │              │              │                         │
```

### Notification Flow (notify_peer)

```
Claude A                  Client A        Relay         Client B                  Claude B
   │                         │              │              │                         │
   │ notify_peer("backend",  │              │              │                         │
   │   "Schema changed!")    │              │              │                         │
   │────────────────────────►│              │              │                         │
   │                         │  send_msg    │              │                         │
   │ (returns immediately)   │─────────────►│              │                         │
   │◄────────────────────────│              │  forward_msg │                         │
   │                         │              │─────────────►│                         │
   │                         │              │              │  tmux send-keys         │
   │                         │              │              │────────────────────────►│
   │                         │              │              │                         │
```

## Session Detection & Output Capture

### How We Detect Claude Sessions

The client daemon needs to identify which tmux sessions are running Claude Code:

1. **Explicit Registration**
   ```bash
   # User explicitly registers
   repowire session register frontend --tmux-session claude-frontend
   ```

2. **Auto-Detection** (stretch goal)
   - Scan tmux sessions
   - Look for Claude Code process
   - Match by working directory

### How We Capture Claude's Response

This is the tricky part. When we inject a query into Claude's session:

1. **Inject the query**
   ```python
   session.send_keys(f"@backend asked: {query}", enter=True)
   ```

2. **Wait for Claude to process**
   - How do we know when Claude is done?
   - Options:
     a. Wait for prompt to reappear
     b. Look for specific markers
     c. Timeout-based with output stabilization
     d. Claude includes end marker in response

3. **Capture the output**
   ```python
   output = pane.capture_pane()
   # Parse to extract just Claude's response
   ```

### Output Parsing Strategy

Claude Code output has structure:
```
╭─────────────────────────────────────────╮
│ Query from @frontend:                    │
│ What's the API schema for users?         │
╰─────────────────────────────────────────╯

The user API schema is:
{
  "id": "string",
  "name": "string",
  "email": "string"
}

> █
```

We need to:
1. Identify where Claude's response starts
2. Identify where it ends (prompt reappears)
3. Extract just the response text

## Implementation Phases

### Phase 1: Local Mesh (Single Machine)

**Goal:** Multiple Claude sessions on same machine can talk to each other.

**Components:**
- Client daemon with local-only mode
- MCP server
- No relay needed (direct tmux access)

**Deliverables:**
- `repowire daemon start`
- `repowire session register <name> --tmux-session <session>`
- MCP tools: `list_peers`, `ask_peer`, `notify_peer`

**Success Criteria:**
- Claude A asks Claude B a question
- Claude B receives it, responds
- Claude A gets the response

### Phase 2: Multi-Machine Mesh

**Goal:** Sessions across machines can communicate.

**Components:**
- Relay server (deployed on your cluster)
- Client daemon connects to relay
- Authentication

**Deliverables:**
- Relay server with WebSocket API
- Client connects to relay
- Auth via API keys

**Success Criteria:**
- Claude on laptop talks to Claude on desktop
- Messages route through relay

### Phase 3: Production Hardening

**Goal:** Ready for real use.

**Components:**
- Robust error handling
- Reconnection logic
- Message persistence (offline delivery)
- Rate limiting
- Monitoring/logging

**Deliverables:**
- Stable relay deployment
- Client handles disconnects gracefully
- Admin dashboard (optional)

### Phase 4: Public Service (repowire.io)

**Goal:** Others can use Repowire.

**Components:**
- Multi-tenant relay
- User registration/auth
- Usage limits/billing
- Documentation

**Deliverables:**
- Public relay at repowire.io
- User onboarding flow
- Docs site

## File Structure

```
repowire/
├── repowire/
│   ├── __init__.py
│   ├── cli.py                 # CLI entry point
│   │
│   ├── client/                # Client daemon
│   │   ├── __init__.py
│   │   ├── daemon.py          # Main daemon process
│   │   ├── session_manager.py # Manages local tmux sessions
│   │   ├── relay_client.py    # WebSocket client to relay
│   │   └── output_parser.py   # Parse Claude output
│   │
│   ├── relay/                 # Relay server
│   │   ├── __init__.py
│   │   ├── server.py          # FastAPI + SocketIO server
│   │   ├── registry.py        # Session registry
│   │   ├── router.py          # Message routing
│   │   └── auth.py            # Authentication
│   │
│   ├── mcp/                   # MCP server
│   │   ├── __init__.py
│   │   └── server.py          # MCP tools
│   │
│   ├── protocol/              # Shared protocol definitions
│   │   ├── __init__.py
│   │   ├── messages.py        # Message types
│   │   └── peers.py           # Peer types
│   │
│   └── transport/             # Transport abstraction
│       ├── __init__.py
│       ├── base.py            # Abstract interface
│       └── tmux.py            # tmux implementation (libtmux)
│
├── tests/
│   ├── test_session_manager.py
│   ├── test_relay.py
│   ├── test_mcp.py
│   └── integration/
│       └── test_e2e.py
│
├── pyproject.toml
├── README.md
├── plan.md                    # This file
└── knowledge-gaps.md          # Open questions
```

## CLI Design

```bash
# Daemon management
repowire daemon start [--relay-url URL] [--api-key KEY]
repowire daemon stop
repowire daemon status

# Session management
repowire session list                    # List local tmux sessions
repowire session register NAME           # Register current tmux session
repowire session register NAME --tmux SESSION  # Register specific session
repowire session unregister NAME

# Peer interaction (for testing)
repowire peer list                       # List all mesh peers
repowire peer ask NAME "query"           # Ask a peer
repowire peer notify NAME "message"      # Notify a peer

# MCP server (called by Claude Code)
repowire mcp                             # Start MCP server (stdio)

# Relay server (for self-hosting)
repowire relay start [--port PORT]
```

## Configuration

```yaml
# ~/.repowire/config.yaml

relay:
  url: "wss://repowire.io"      # or self-hosted URL
  api_key: "rw_..."             # API key for authentication

daemon:
  auto_discover: true           # Auto-discover Claude sessions
  register_on_start: true       # Auto-register when daemon starts

sessions:
  # Pre-configured session mappings
  frontend:
    tmux_session: "claude-frontend"
    path: "/Users/prass/app/frontend"
  backend:
    tmux_session: "claude-backend"
    path: "/Users/prass/app/backend"

logging:
  level: info
  file: ~/.repowire/repowire.log
```

## Security Considerations

1. **Authentication**
   - API keys per user
   - Keys stored securely (not in plain text config)
   - Token refresh mechanism

2. **Authorization**
   - Users can only see their own peers
   - No cross-user message routing

3. **Transport Security**
   - WSS (WebSocket Secure) only
   - TLS 1.3

4. **Message Security** (optional enhancement)
   - E2E encryption of message payloads
   - Only endpoints can decrypt

5. **Rate Limiting**
   - Per-user message limits
   - Prevent abuse

## Success Metrics

1. **Latency**: Query → Response < 5 seconds (for simple queries)
2. **Reliability**: 99.9% message delivery
3. **Adoption**: Can onboard new user in < 5 minutes
4. **Scale**: Support 100+ concurrent peers per user

## Concrete Implementation Details

### libtmux Integration (Researched)

Based on [libtmux documentation](https://libtmux.git-pull.com/api/panes.html):

```python
import libtmux

class TmuxSessionManager:
    def __init__(self):
        self.server = libtmux.Server()

    def send_to_session(self, session_name: str, text: str) -> None:
        """Inject text into a tmux session."""
        session = self.server.sessions.get(session_name=session_name)
        pane = session.active_window.active_pane
        pane.send_keys(text, enter=True)

    def capture_output(self, session_name: str, lines: int = 100) -> list[str]:
        """Capture recent output from a tmux session."""
        session = self.server.sessions.get(session_name=session_name)
        pane = session.active_window.active_pane
        return pane.capture_pane(start=-lines, end=-1)

    def list_sessions(self) -> list[dict]:
        """List all tmux sessions."""
        return [
            {"name": s.name, "id": s.id, "windows": len(s.windows)}
            for s in self.server.sessions
        ]
```

### Response Detection - Claude Code Hooks

Instead of polling for output, we use [Claude Code hooks](https://docs.claude.com/en/docs/claude-code/hooks-guide) which fire deterministically:

**Key hooks for Repowire:**
- **Stop** - Fires when Claude finishes responding
- **UserPromptSubmit** - Fires when input is received (track peer queries)
- **Notification** - Forward notifications to peers

**Hook configuration (~/.claude/settings.json):**
```json
{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "repowire hook stop"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "repowire hook prompt-submit"
          }
        ]
      }
    ]
  }
}
```

**Flow with hooks:**
```
1. Peer A sends query to Peer B via relay
2. Repowire daemon injects query into B's tmux session
3. Claude B processes the query
4. Stop hook fires → `repowire hook stop`
5. Hook script captures output and sends response to daemon
6. Daemon sends response back through relay to Peer A
```

**Hook handler implementation:**
```python
# repowire/hooks/handler.py
import json
import sys
from pathlib import Path

def handle_stop():
    """Called when Claude finishes responding."""
    # Read hook data from stdin
    data = json.load(sys.stdin)

    # Check if this was a peer query (we track pending queries)
    pending_file = Path.home() / ".repowire" / "pending_query.json"
    if pending_file.exists():
        pending = json.loads(pending_file.read_text())

        # Capture response (data contains session info)
        # Notify daemon to send response
        notify_daemon("response_ready", {
            "correlation_id": pending["correlation_id"],
            "session_id": data.get("session_id"),
        })

        pending_file.unlink()  # Clear pending query

def notify_daemon(event: str, data: dict):
    """Notify the Repowire daemon via Unix socket."""
    import socket
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect("/tmp/repowire.sock")
    sock.send(json.dumps({"event": event, "data": data}).encode())
    sock.close()
```

**Benefits over polling:**
- Deterministic - no guessing when Claude is done
- Zero latency - hook fires immediately when done
- Native integration with Claude Code lifecycle

### Relay Server Stack (Researched)

Based on [python-socketio](https://python-socketio.readthedocs.io/en/latest/server.html) and [fastapi-socketio](https://github.com/pyropy/fastapi-socketio):

```python
from fastapi import FastAPI
from fastapi_socketio import SocketManager
from pydantic import BaseModel

app = FastAPI(title="Repowire Relay")
sio = SocketManager(app=app)

# In-memory registry (Redis later for scaling)
peers: dict[str, dict] = {}  # sid -> peer info
user_rooms: dict[str, set[str]] = {}  # user_id -> set of peer sids

class PeerInfo(BaseModel):
    name: str
    path: str
    machine: str
    user_id: str

@sio.on("register")
async def handle_register(sid: str, data: dict):
    peer = PeerInfo(**data)
    peers[sid] = peer.model_dump()

    # Add to user's room for isolation
    if peer.user_id not in user_rooms:
        user_rooms[peer.user_id] = set()
    user_rooms[peer.user_id].add(sid)

    # Notify others in same user's room
    await sio.emit(
        "peer_online",
        {"name": peer.name, "path": peer.path},
        room=peer.user_id,
        skip_sid=sid
    )

    # Join user's room
    sio.enter_room(sid, peer.user_id)

@sio.on("send_message")
async def handle_message(sid: str, data: dict):
    target_name = data["target"]
    payload = data["payload"]
    sender = peers.get(sid)

    if not sender:
        return {"error": "Not registered"}

    # Find target peer
    target_sid = None
    for peer_sid, peer_info in peers.items():
        if (peer_info["name"] == target_name and
            peer_info["user_id"] == sender["user_id"]):
            target_sid = peer_sid
            break

    if not target_sid:
        return {"error": "Peer not found"}

    # Forward message
    await sio.emit("message", {
        "from": sender["name"],
        "payload": payload,
        "correlation_id": data.get("correlation_id")
    }, to=target_sid)

    return {"status": "sent"}

@sio.on("disconnect")
async def handle_disconnect(sid: str):
    peer = peers.pop(sid, None)
    if peer:
        await sio.emit(
            "peer_offline",
            {"name": peer["name"]},
            room=peer["user_id"]
        )
```

### Message Protocol

```python
from pydantic import BaseModel
from typing import Literal
from uuid import uuid4
from datetime import datetime

class Message(BaseModel):
    id: str = Field(default_factory=lambda: str(uuid4()))
    type: Literal["query", "response", "notification", "broadcast"]
    from_peer: str
    to_peer: str | None  # None for broadcast
    payload: dict
    correlation_id: str | None = None  # For request/response matching
    timestamp: datetime = Field(default_factory=datetime.utcnow)

# Query: expects response
{
    "type": "query",
    "from_peer": "frontend",
    "to_peer": "backend",
    "payload": {"text": "What's the API schema for users?"},
    "correlation_id": "abc123"
}

# Response: matches query
{
    "type": "response",
    "from_peer": "backend",
    "to_peer": "frontend",
    "payload": {"text": "{id, name, email}", "success": true},
    "correlation_id": "abc123"  # Matches query
}

# Notification: fire and forget
{
    "type": "notification",
    "from_peer": "frontend",
    "to_peer": "backend",
    "payload": {"text": "FYI: Updated the login form"}
}

# Broadcast: to all peers
{
    "type": "broadcast",
    "from_peer": "frontend",
    "to_peer": null,
    "payload": {"text": "Starting deployment in 5 minutes"}
}
```

## Open Questions

See `knowledge-gaps.md` for detailed open questions and research needed.

Key remaining unknowns:
1. What data does the Stop hook receive? (need to test)
2. How to correlate injected queries with their responses
3. Handling very long-running Claude operations (> 2 min)
