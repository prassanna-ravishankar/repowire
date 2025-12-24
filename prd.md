---
name: Repowire Lateral Mesh
overview: Repowire is a pure MCP server that enables peer-to-peer communication between isolated coding sessions (Happy, OpenCode). It leverages existing transport mechanisms - Happy's WebSocket protocol and OpenCode's SDK - to inject messages into sessions, acting as the missing "lateral layer" for multi-agent coordination.
todos:
  - id: phase1-mcp
    content: Create core MCP server with FastMCP, peer registry, and state persistence
    status: done
  - id: phase1-tools
    content: Implement register, unregister, list_peers, read_state, write_state tools
    status: done
  - id: phase1-cli
    content: Add 'repowire mesh' CLI command to start MCP server
    status: done
  - id: phase2-opencode
    content: Create OpenCode transport adapter using opencode-ai SDK
    status: done
  - id: phase2-ask-peer
    content: Wire ask_peer tool to OpenCode transport, test with two sessions
    status: done
  - id: phase3-happy-ws
    content: Create Happy WebSocket transport with encryption
    status: done
  - id: phase3-happy-auth
    content: Add 'repowire auth happy' command for authentication
    status: done
  - id: phase3-happy-test
    content: Test ask_peer with Happy sessions (requires live sessions)
    status: pending
  - id: phase4-cross
    content: Test and fix cross-agent communication (Happy <-> OpenCode, requires live sessions)
    status: pending
  - id: phase4-files
    content: Implement read_peer_file for both transports
    status: done
  - id: phase5-status
    content: Add 'repowire status' command
    status: done
  - id: phase5-cleanup
    content: Remove old daemon.py, process.py, config.py, bus.py files
    status: done
---

# Repowire: Lateral Mesh for Coding Agents

## Problem Statement

Modern AI coding agents (Happy, OpenCode, Cursor) operate in isolation. Each session:

- Syncs with its own backend (Happy Cloud, OpenCode server)
- Can receive messages from mobile apps or web UIs
- **Cannot communicate with peer sessions**

A developer working on a full-stack feature needs agents in `backend/`, `frontend/`, and `infra/` repos to coordinate. Today, the human must manually copy context between sessions.

## Solution

Repowire is a **pure MCP server** that adds lateral communication between sessions. It does NOT:

- Spawn or manage agent processes
- Require configuration files
- Invent new protocols

It DOES:

- Expose MCP tools (`register`, `ask_peer`, `read_peer_file`, etc.)
- Route messages between peers using **existing transport mechanisms**
- Maintain a peer registry and shared state

## Architecture

```javascript
┌─────────────────────────────────────────────────────────────────┐
│                      Repowire MCP Server                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐   │
│  │ Peer Registry│  │ Shared State │  │ Transport Adapters   │   │
│  │              │  │ (local JSON) │  │ ┌──────┐ ┌─────────┐ │   │
│  │ frontend: {} │  │              │  │ │Happy │ │OpenCode │ │   │
│  │ backend: {}  │  │ key: value   │  │ │WebSkt│ │  SDK    │ │   │
│  └──────────────┘  └──────────────┘  │ └──────┘ └─────────┘ │   │
│                                       └──────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
         ▲                    ▲                    │
         │ MCP                │ MCP                │ Transport
         │                    │                    ▼
    ┌────┴────┐          ┌────┴────┐         ┌─────────────┐
    │Session A│          │Session B│         │Happy Cloud /│
    │(frontend)│          │(backend)│         │OpenCode API │
    └─────────┘          └─────────┘         └─────────────┘
```



## Transport Adapters

Repowire uses existing, proven mechanisms to send messages to sessions:

### Happy Transport

Happy sessions connect to Happy Cloud via WebSocket. The mobile app uses this same connection to send messages. Repowire authenticates with Happy Cloud and uses the identical protocol:

```python
# Reference: happy/sources/sync/sync.ts line 287
# apiSocket.send('message', { sid, message, localId, sentFrom, permissionMode })

class HappyTransport:
    async def send_message(self, session_id: str, text: str):
        encrypted = await self.encrypt(text)
        await self.socket.emit('message', {
            'sid': session_id,
            'message': encrypted,
            'localId': uuid4(),
            'sentFrom': 'repowire',
            'permissionMode': 'default'
        })
```

Reference implementation: [`happy/sources/sync/apiSocket.ts`](happy/sources/sync/apiSocket.ts)

### OpenCode Transport

OpenCode exposes a Python SDK for session management:

```python
# Reference: https://github.com/sst/opencode-sdk-python

from opencode_ai import AsyncOpencode

class OpenCodeTransport:
    def __init__(self):
        self.client = AsyncOpencode()
    
    async def send_message(self, session_id: str, text: str) -> str:
        response = await self.client.session.chat(
            session_id,
            parts=[{"type": "text", "text": text}]
        )
        return self.extract_response_text(response)
```



## MCP Tools Specification

### `register`

Called by a session when it connects to Repowire.

```javascript
Parameters:
    - name: str           # Human-readable name, e.g., "backend"
    - agent_type: str     # "happy" or "opencode"
    - session_id: str     # The actual session ID for routing
    - path: str           # Working directory, e.g., "/Users/dev/backend"
    - capabilities: list  # Optional hints, e.g., ["api", "database"]

Returns:
    - success: bool
    - peers: list         # Currently registered peers
```



### `unregister`

Called when a session disconnects.

```javascript
Parameters:
    - name: str

Returns:
    - success: bool
```



### `list_peers`

Discover other sessions in the mesh.

```javascript
Parameters: none

Returns:
    - peers: list[{name, agent_type, path, capabilities, is_active}]
    - self: str           # Caller's registered name
```



### `ask_peer`

Send a query to another session and wait for a response.

```javascript
Parameters:
    - target: str         # Peer name, e.g., "backend"
    - query: str          # The question to ask

Returns:
    - response: str       # The peer's answer

Errors:
    - PeerNotFound        # Target not registered
    - PeerTimeout         # No response within timeout
    - PeerError           # Peer returned an error
```

Implementation notes:

- Inject query as a user message via the appropriate transport
- The query should be prefixed to indicate it's from a peer: `[PEER QUERY from frontend]: {query}`
- Wait for the next agent response, extract text, return it
- Default timeout: 60 seconds

### `notify_peer`

Send a notification to a peer (fire-and-forget).

```javascript
Parameters:
    - target: str
    - message: str

Returns:
    - success: bool
```



### `broadcast`

Notify all peers.

```javascript
Parameters:
    - message: str

Returns:
    - notified: list[str]   # Names of peers notified
```



### `read_peer_file`

Read a file from a peer's working directory.

```javascript
Parameters:
    - target: str
    - file_path: str      # Relative to peer's working directory

Returns:
    - content: str

Errors:
    - PeerNotFound
    - FileNotFound
    - AccessDenied
```

Implementation: Use session RPC or bash command via the transport.

### `read_state` / `write_state`

Shared key-value state accessible to all peers.

```javascript
# write_state
Parameters:
    - key: str
    - value: any          # JSON-serializable

# read_state  
Parameters:
    - key: str (optional, omit for all)

Returns:
    - value: any (or dict of all if key omitted)
```

State is persisted to `~/.repowire/state.json`.

## File Structure

```javascript
repowire/
├── __init__.py
├── cli.py                  # Entry point: `repowire mesh`, `repowire status`
├── mesh/
│   ├── __init__.py
│   ├── server.py           # MCP server setup using FastMCP
│   ├── tools.py            # MCP tool implementations
│   ├── peers.py            # Peer registry
│   └── state.py            # Shared state persistence
├── transport/
│   ├── __init__.py
│   ├── base.py             # Abstract transport interface
│   ├── happy.py            # Happy Cloud WebSocket transport
│   └── opencode.py         # OpenCode SDK transport
└── protocol.py             # Message types, serialization
```



## Files to Remove

The following files from the original implementation should be removed as they represent the "orchestrator" approach:

- `repowire/daemon.py` - Process orchestration
- `repowire/process.py` - Subprocess management  
- `repowire/config.py` - YAML config parsing (keep minimal version for auth credentials only)
- `repowire/bus.py` - Internal message bus (replaced by transports)
- `repowire/blackboard.py` - Move to `mesh/state.py`
- `repowire/mcp/server.py` - Replace with `mesh/server.py`
- `repowire/tui/` - Optional, can be kept for status display

## CLI Commands

### `repowire mesh`

Start the MCP server.

```bash
repowire mesh [--port 9876] [--state-file ~/.repowire/state.json]
```

Output:

```javascript
Repowire mesh listening on http://localhost:9876
MCP endpoint: http://localhost:9876/mcp
Waiting for peers to connect...
```



### `repowire status`

Show connected peers and state.

```bash
repowire status [--port 9876]
```

Output:

```javascript
Repowire Mesh Status
====================
Peers (3 connected):
  backend   (happy)    /Users/dev/api      [api, database]  active
  frontend  (happy)    /Users/dev/web      [ui]             active  
  infra     (opencode) /Users/dev/terraform [iac]           idle

Shared State (2 keys):
  api_version: "2.1.0"
  deploy_target: "staging"
```



### `repowire auth`

Authenticate with Happy Cloud (required for Happy transport).

```bash
repowire auth happy
# Opens QR code flow or prompts for token
# Stores credentials in ~/.repowire/credentials.json
```



## Authentication

### Happy

Repowire needs Happy credentials to connect to Happy Cloud. Two options:

1. **QR Code Flow**: Same as mobile app onboarding (see `happy/sources/auth/`)
2. **Token Import**: User provides token and secret from existing Happy login

Store in `~/.repowire/credentials.json`:

```json
{
  "happy": {
    "token": "...",
    "secret": "..."
  }
}
```



### OpenCode

OpenCode SDK uses environment variables or config:

- `OPENCODE_BASE_URL` - Server URL (default: localhost)
- Or pass `base_url` to `AsyncOpencode()`

## How Agents Connect

Agents add Repowire as an MCP server in their configuration:

### Happy CLI

```json
// ~/.happy/mcp.json (or equivalent config location)
{
  "mcpServers": {
    "repowire": {
      "command": "repowire",
      "args": ["mcp-stdio"]
    }
  }
}
```



### OpenCode

```json
// opencode mcp config
{
  "mcpServers": {
    "repowire": {
      "url": "http://localhost:9876/mcp"
    }
  }
}
```



### Cursor

```json
// .cursor/mcp.json
{
  "mcpServers": {
    "repowire": {
      "command": "repowire",
      "args": ["mcp-stdio"]
    }
  }
}
```



## Implementation Sequence

### Phase 1: Core MCP Server

1. Create `mesh/server.py` with FastMCP
2. Implement `peers.py` - in-memory peer registry
3. Implement `state.py` - JSON file persistence
4. Implement basic tools: `register`, `unregister`, `list_peers`, `read_state`, `write_state`
5. Add `repowire mesh` CLI command

### Phase 2: OpenCode Transport

1. Create `transport/opencode.py` using `opencode-ai` SDK
2. Implement `send_message` and `wait_for_response`
3. Wire up `ask_peer` tool to use OpenCode transport
4. Test with two OpenCode sessions

### Phase 3: Happy Transport

1. Create `transport/happy.py` with WebSocket client
2. Port encryption logic from `happy/sources/sync/encryption/`
3. Implement `send_message` using Happy's protocol
4. Implement response listening via sync updates
5. Add `repowire auth happy` command
6. Test with two Happy sessions

### Phase 4: Cross-Agent Communication

1. Test Happy session querying OpenCode session
2. Test OpenCode session querying Happy session
3. Implement `read_peer_file` for both transports
4. Add `notify_peer` and `broadcast`

### Phase 5: Polish

1. Add `repowire status` command
2. Improve error handling and timeouts
3. Add logging and debugging output
4. Write documentation

## Testing Strategy

### Unit Tests

- Peer registry add/remove/lookup
- State persistence load/save
- Message serialization

### Integration Tests

- Start mesh, register mock peer, verify registration
- Send message via OpenCode transport, verify delivery
- Send message via Happy transport, verify delivery

### End-to-End Tests

- Start mesh + two OpenCode sessions
- Session A calls `ask_peer("B", "hello")`, verify B receives, A gets response
- Same for Happy sessions
- Cross-agent: Happy asks OpenCode

## Dependencies

```toml
# pyproject.toml additions
[project]
dependencies = [
    "mcp",                    # MCP server library (FastMCP)
    "opencode-ai",            # OpenCode SDK
    "python-socketio[client]", # WebSocket for Happy
    "pynacl",                 # Encryption for Happy (tweetnacl equivalent)
    "httpx",                  # HTTP client
    "click",                  # CLI framework (existing)
    "rich",                   # Terminal output (existing)
]
```



## Open Questions

1. **Response Extraction**: When we inject a query into a session, how do we know which response is "ours"? Options:

- Correlate by timing (fragile)
- Add correlation ID in query, expect it in response
- Use a special response format agents are trained to use

2. **Concurrent Queries**: If Session A has multiple pending queries to Session B, how do we match responses? May need request/response IDs.
3. **Session Discovery**: Should Repowire auto-discover running sessions, or rely entirely on explicit registration?