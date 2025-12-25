# Knowledge Gaps

This document tracks open questions, unknowns, and research needed for Repowire.

**Legend:**
- 🔴 **Critical** - Blocks implementation
- 🟡 **Important** - Affects design decisions
- 🟢 **Nice to Know** - Optimization/polish

---

## 1. Claude Output Detection

### 🔴 How do we know when Claude is "done" responding?

**Context:** When we inject a query into Claude's tmux session, we need to know when Claude has finished responding so we can capture the output and send it back.

**Options considered:**
1. **Wait for prompt to reappear** - Look for `> ` or similar
2. **Output stabilization** - Wait until output stops changing for N seconds
3. **Marker-based** - Ask Claude to end responses with a specific marker
4. **Process monitoring** - Watch for Claude process to become idle

**Questions:**
- What does Claude Code's prompt look like exactly?
- Does it vary based on context (thinking, tool use, etc.)?
- Can we reliably detect it via tmux capture?

**Status:** ❓ Unresolved

**Research needed:**
- [ ] Examine Claude Code's actual terminal output patterns
- [ ] Test output stabilization approach
- [ ] Check if there's an API or signal for "done"

---

### 🔴 How do we distinguish Claude's response from other output?

**Context:** The tmux pane contains everything - our injected query, Claude's thinking, tool outputs, the response, and the new prompt.

**Questions:**
- What's the exact format of Claude Code's output?
- Are there ANSI escape codes we need to handle?
- How do we parse out just the response?

**Status:** ❓ Unresolved

**Research needed:**
- [ ] Capture raw tmux output with escape codes
- [ ] Document Claude Code's output structure
- [ ] Build parser for extracting responses

---

## 2. libtmux / tmux Interaction

### 🟡 What's the best libtmux pattern for output capture?

**Context:** Need to capture Claude's output reliably.

**Known:**
- `pane.capture_pane()` gets current visible content
- Can specify start/end lines
- `-p` flag for plain text vs escape codes

**Questions:**
- Should we capture with or without escape codes?
- How much history should we capture?
- Performance implications of frequent capture?

**Status:** 🔄 Partially understood

**Research needed:**
- [ ] Review libtmux documentation
- [ ] Test capture performance
- [ ] Determine optimal capture strategy

---

### 🟡 How do we inject text that looks like a peer query?

**Context:** When forwarding a message from another peer, we need Claude to understand it's from a peer, not the user.

**Options:**
1. **Prefix format:** `@backend asks: What's the schema?`
2. **Special markers:** `[PEER:backend] What's the schema?`
3. **System message:** Somehow inject as system context

**Questions:**
- How will Claude interpret injected text?
- Should we use a specific format?
- Can Claude distinguish peer queries from user input?

**Status:** ❓ Unresolved

**Research needed:**
- [ ] Test different injection formats with Claude
- [ ] See if Claude can be prompted to handle peer messages

---

### 🟢 Can we use tmux hooks or watchers instead of polling?

**Context:** Instead of polling for output changes, maybe tmux has hooks.

**Questions:**
- Does tmux support output hooks?
- Can we watch for pane content changes?
- Would this be more efficient than polling?

**Status:** ❓ Unresolved

**Research needed:**
- [ ] Review tmux hooks documentation
- [ ] Check if libtmux exposes hooks

---

## 3. Relay Server

### 🟡 What WebSocket library for Python relay?

**Options:**
1. **python-socketio + FastAPI** - Familiar, good ecosystem
2. **websockets + FastAPI** - Lighter weight
3. **Starlette native** - Minimal dependencies

**Considerations:**
- Need rooms/namespaces for user isolation
- Need reliable message delivery
- Should handle reconnection gracefully

**Status:** ⚡ Decision needed

**Leaning toward:** python-socketio (Socket.IO protocol has good features)

---

### 🟡 Redis vs in-memory for session registry?

**Context:** Need to store which peers are online.

**Options:**
1. **In-memory dict** - Simple, but lost on restart
2. **Redis** - Persistent, pub/sub for scaling
3. **SQLite** - Persistent, no extra service

**Considerations:**
- Single server initially, may scale later
- Need pub/sub for multi-process?
- Session state is ephemeral (can rebuild on reconnect)

**Status:** ⚡ Decision needed

**Leaning toward:** In-memory initially, Redis later if needed

---

### 🟡 Authentication mechanism?

**Options:**
1. **API keys** - Simple, static
2. **JWT tokens** - Can include claims, expiry
3. **OAuth** - If we want social login later

**Questions:**
- How do users get their API key?
- Do we need refresh tokens?
- Per-machine or per-user keys?

**Status:** ⚡ Decision needed

**Leaning toward:** API keys for simplicity (like Anthropic API)

---

## 4. Message Protocol

### 🟡 What's the message format between peers?

**Draft:**
```json
{
  "id": "msg_abc123",
  "type": "query",           // query, response, notification, broadcast
  "from": "frontend",
  "to": "backend",           // null for broadcast
  "payload": {
    "text": "What's the API schema?",
    "context": {}            // optional metadata
  },
  "timestamp": 1703000000,
  "correlation_id": "..."    // for request/response matching
}
```

**Questions:**
- What metadata should we include?
- How do we handle binary payloads (files)?
- Should we support structured responses (JSON)?

**Status:** 🔄 Draft exists

---

### 🟡 How do we handle request/response correlation?

**Context:** When Claude A asks Claude B a question, we need to match the response back.

**Options:**
1. **Correlation ID** - Include ID in request, response echoes it
2. **Sequence numbers** - Track per-peer
3. **Callbacks** - Store pending callbacks, resolve on response

**Status:** ⚡ Decision needed

**Leaning toward:** Correlation ID (simple, stateless)

---

## 5. Error Handling

### 🟡 What if the target peer is offline?

**Options:**
1. **Immediate error** - "Peer offline" error
2. **Store and forward** - Queue message, deliver when online
3. **Timeout** - Wait N seconds, then error

**Questions:**
- Do we need message persistence?
- How long to wait for peer?
- Should queries and notifications behave differently?

**Status:** ⚡ Decision needed

**Leaning toward:** Immediate error for queries, store-and-forward optional for notifications

---

### 🟡 What if Claude times out or crashes?

**Context:** We injected a query but Claude never responds.

**Options:**
1. **Timeout** - Return error after N seconds
2. **Health check** - Monitor Claude process
3. **Retry** - Retry once before failing

**Questions:**
- What's a reasonable timeout?
- How do we detect Claude crash vs slow response?

**Status:** ⚡ Decision needed

---

## 6. User Experience

### 🟢 How does a user set up a new session?

**Ideal flow:**
1. Start Claude in tmux: `tmux new -s frontend && claude`
2. Register: `repowire session register frontend`
3. Done - session is now in mesh

**Questions:**
- Can we auto-detect Claude sessions?
- Should registration be automatic?
- What about non-tmux setups (future)?

**Status:** 🔄 Draft exists

---

### 🟢 How do peers know about each other's capabilities?

**Context:** Maybe frontend Claude shouldn't ask infra Claude about React patterns.

**Options:**
1. **Metadata in registration** - Tags, description
2. **Discovery queries** - Ask "what can you help with?"
3. **Central directory** - Maintain capability map

**Status:** ❓ Not started (Phase 2+)

---

## 7. Transport Abstraction

### 🟢 What if user doesn't use tmux?

**Possible backends:**
- tmux (primary)
- screen (legacy)
- Direct PTY (advanced)
- VS Code extension (future)
- Cursor integration (future)

**Questions:**
- How do we abstract the session interface?
- What's the minimal interface needed?

**Status:** ❓ Future consideration

**Draft interface:**
```python
class SessionBackend(ABC):
    def send_input(self, session_id: str, text: str) -> None: ...
    def capture_output(self, session_id: str) -> str: ...
    def list_sessions(self) -> list[Session]: ...
    def is_ready(self, session_id: str) -> bool: ...
```

---

## 8. Scale & Performance

### 🟢 How many concurrent peers can we support?

**Considerations:**
- WebSocket connections per relay instance
- Message throughput
- Memory for session state

**Status:** ❓ Unknown (need to test)

---

### 🟢 Should messages be encrypted end-to-end?

**Context:** Currently relay can see message contents.

**Options:**
1. **No E2E** - Trust relay (simpler)
2. **Optional E2E** - User can enable
3. **Always E2E** - Maximum privacy

**Questions:**
- Key distribution mechanism?
- Performance overhead?

**Status:** ❓ Future consideration (Phase 3+)

---

## Research Log

### 2024-12-25: Initial gap identification

Created this document. Key blockers:
1. Claude output detection
2. libtmux capture patterns
3. Message protocol design

### 2024-12-25: libtmux Research

**Findings from [libtmux documentation](https://libtmux.git-pull.com/api/panes.html):**

**`send_keys()` method:**
```python
pane.send_keys(cmd, enter=True, suppress_history=False, literal=False)
```
- `enter=True` sends Enter key after text
- `literal=True` sends characters literally (useful for special chars)
- `suppress_history=True` prepends space to avoid shell history

**`capture_pane()` method:**
```python
pane.capture_pane(start=None, end=None, escape_sequences=False, ...)
```
- Returns `list[str]` of captured lines
- `start/end` can be integers (line numbers) or `-` for history
- `escape_sequences=True` includes ANSI codes
- Negative line numbers access scrollback history

**Practical pattern:**
```python
pane.send_keys("query text", enter=True)
# Wait for response...
output = pane.capture_pane()  # Returns list of strings
```

### 2024-12-25: Claude Code Output Detection

**Findings from [Claude Code docs](https://code.claude.com/docs/en/cli-reference):**

Claude Code has two modes:
1. **Interactive mode** - REPL with prompt, slash commands
2. **Print mode** (`-p`) - Single shot, exits after response

For interactive mode, detection options:
- `Ctrl+C` cancels current generation
- When done, terminal accepts input again
- Verbose mode (`Ctrl+O`) shows detailed execution

**Proposed detection strategy:**
1. **Output stabilization** - Wait until no new output for 2-3 seconds
2. **Prompt pattern matching** - Look for input prompt (need to determine exact format)
3. **Process state** - Check if Claude process is waiting for input (via `/proc` or `ps`)

**Still need:** Actual Claude Code prompt format in terminal

### 2024-12-25: Relay Server Stack

**Findings from [python-socketio docs](https://python-socketio.readthedocs.io/en/latest/server.html):**

- Can integrate with FastAPI via `socketio.ASGIApp`
- Supports rooms and namespaces for user isolation
- For multiple servers: use Redis pub/sub adapter
- Recommended: `transports=['websocket']` (no long-polling)

**Stack decision:** FastAPI + python-socketio + Redis (later)

**Example integration via [fastapi-socketio](https://github.com/pyropy/fastapi-socketio):**
```python
from fastapi import FastAPI
from fastapi_socketio import SocketManager

app = FastAPI()
sio = SocketManager(app=app)

@sio.on('message')
async def handle_message(sid, data):
    await sio.emit('response', {'data': 'received'}, to=sid)
```

---

## Resolved Gaps

### ✅ libtmux API for capture/send

**Resolution:** Documented above. Key methods:
- `pane.send_keys(text, enter=True)`
- `pane.capture_pane()` returns `list[str]`

### ✅ Relay server WebSocket library

**Resolution:** Use FastAPI + python-socketio
- Good ecosystem, rooms/namespaces support
- Can scale with Redis adapter later
- `fastapi-socketio` package simplifies integration

---

## Still Open

### 🟢 Claude Code exact prompt format

Low priority now - we have a better solution with hooks.

---

## Resolved Gaps

### ✅ Response end detection - USE HOOKS!

**Resolution:** Claude Code has a **Stop hook** that fires when Claude finishes responding!

From [Claude Code hooks documentation](https://docs.claude.com/en/docs/claude-code/hooks-guide):

**Available hooks:**
- **PreToolUse** - Before tool calls (can block)
- **PostToolUse** - After tool calls complete
- **UserPromptSubmit** - When user submits prompt
- **Notification** - When Claude sends notifications
- **Stop** - When Claude finishes responding ← **THIS IS IT**
- **SubagentStop** - When subagent tasks complete
- **SessionStart** - When session starts
- **SessionEnd** - When session ends

**Configuration (~/.claude/settings.json):**
```json
{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "repowire-hook stop"
          }
        ]
      }
    ]
  }
}
```

**New architecture:**

Instead of polling tmux for output stabilization:
1. Configure Stop hook → calls `repowire-hook stop`
2. Hook script notifies the Repowire daemon that Claude is done
3. Daemon captures output and sends response

**Benefits:**
- Deterministic (no guessing when Claude is done)
- Works with Claude's native lifecycle
- Can also use other hooks (Notification, SubagentStop)

**We can also use:**
- **UserPromptSubmit** - Know when a peer query was injected
- **Notification** - Forward Claude's notifications to peers
