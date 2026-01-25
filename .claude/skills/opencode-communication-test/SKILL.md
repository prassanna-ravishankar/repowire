---
name: opencode-communication-test
description: Integration test for repowire OpenCode backend peer-to-peer messaging. Tests WebSocket plugin connection, peer registration, and inter-agent queries via session.prompt() injection.
---

# OpenCode Communication Test

Integration test for OpenCode backend peer-to-peer communication via WebSocket plugin connections.

## Prerequisites

- OpenCode CLI installed (`opencode`)
- tmux installed (for visual test sessions)
- repowire daemon configured for opencode backend

## Phase 1: Environment Discovery

Gather current state before presenting test plan.

### 1.1 Check existing tmux sessions
```bash
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
```

### 1.2 Check if opencode is available
```bash
which opencode || echo "opencode not found in PATH"
```

### 1.3 Check daemon status and backend
```bash
curl -s http://127.0.0.1:8377/health 2>/dev/null | jq . || echo "Daemon not running"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | {name, status}' || echo "No peers"
```

### 1.4 Check existing repowire installations
```bash
# Check for uv tool installation
uv tool list 2>/dev/null | grep repowire || echo "No uv tool installation"

# Check for plugin installation
ls -la ~/.config/opencode/plugin/repowire.ts 2>/dev/null || echo "Plugin not installed"
```

### 1.5 Fresh Install (Ask User First)

**Ask the user**: "Do you want to remove existing repowire installations for a fresh test? This will run:
- `repowire uninstall` (remove plugin)
- `uv tool uninstall repowire` (remove global tool)
- Then reinstall from local dev code"

If user confirms:
```bash
# Stop daemon if running
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null || true
sleep 1

# Uninstall plugin
repowire uninstall 2>/dev/null || true

# Uninstall uv tool
uv tool uninstall repowire 2>/dev/null || true

# Install fresh from local dev
repowire setup --dev --backend opencode
```

## Phase 2: Present Test Plan for Confirmation

**Before executing any test steps**, present the full plan to the user:

### Test Plan Template

```
=== REPOWIRE OPENCODE COMMUNICATION TEST PLAN ===

TMUX SESSION TO CREATE:
  • opencode-test (with windows for each peer)

TEST PROJECTS (ask user for 2 directories):
  • peer-1: <PROJECT_1>
  • peer-2: <PROJECT_2>

TESTS TO RUN:
  1. WebSocket connection test
     Expected: Plugins connect and register with daemon

  2. Peer discovery test
     Expected: Both peers visible in list_peers

  3. Bidirectional query test (peer-1 → peer-2)
     Expected: Query injected via session.prompt(), response returned

  4. Reverse query test (peer-2 → peer-1)
     Expected: Query injected via session.prompt(), response returned

  5. Status tracking test
     Expected: Peer shows "busy" during processing, "online" after

COMMANDS TO EXECUTE:
  # Start daemon with opencode backend
  repowire serve --backend opencode

  # Install plugin
  repowire setup --backend opencode

  # Create tmux session
  tmux new-session -d -s opencode-test -n peer-1
  tmux new-window -t opencode-test -n peer-2

  # Start opencode in each window
  tmux send-keys -t opencode-test:peer-1 "cd <PROJECT_1> && opencode" Enter
  tmux send-keys -t opencode-test:peer-2 "cd <PROJECT_2> && opencode" Enter

CLEANUP:
  tmux kill-session -t opencode-test
  repowire peer prune --force

Proceed with test? [y/N]
```

**Wait for user confirmation before proceeding.**

## Phase 3: Test Environment Setup

### 3.1 Start daemon with opencode backend
```bash
# Stop any existing daemon
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null || true
sleep 1

# Start with opencode backend
repowire serve --backend opencode &
sleep 2
```

### 3.2 Verify daemon is running with opencode backend
```bash
curl -s http://127.0.0.1:8377/health | jq '.backend'
# Expected: "opencode"
```

### 3.3 Install/verify plugin
```bash
repowire setup --backend opencode
cat ~/.config/opencode/plugin/repowire.ts | head -20
```

### 3.4 Create tmux session with windows
```bash
# Create session with two windows
tmux new-session -d -s opencode-test -n peer-1
tmux new-window -t opencode-test -n peer-2
```

### 3.5 Navigate to projects and start opencode
```bash
tmux send-keys -t opencode-test:peer-1 "cd $PROJECT_1" Enter
tmux send-keys -t opencode-test:peer-2 "cd $PROJECT_2" Enter
sleep 1

# Start opencode sessions
tmux send-keys -t opencode-test:peer-1 "opencode" Enter
tmux send-keys -t opencode-test:peer-2 "opencode" Enter

# Wait for plugin to connect
sleep 5
```

### 3.6 Verify WebSocket connections
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, status}'
```

Expected:
- `$PEER_1_NAME` (folder name of PROJECT_1) - status "online"
- `$PEER_2_NAME` (folder name of PROJECT_2) - status "online"

## Phase 4: Peer Discovery Test

### 4.1 Test list_peers from peer-1
```bash
tmux send-keys -t opencode-test:peer-1 "Use the list_peers tool to show all connected peers" Enter
tmux send-keys -t opencode-test:peer-1 Enter
sleep 10
tmux capture-pane -t opencode-test:peer-1 -p -S -50 | tail -30
```

**Expected**: JSON array showing both peers with their status.

## Phase 5: Bidirectional Query Test

### 5.1 Send query from peer-1 to peer-2
```bash
tmux send-keys -t opencode-test:peer-1 "Use the ask_peer tool to ask $PEER_2_NAME what their project is about" Enter
tmux send-keys -t opencode-test:peer-1 Enter
```

### 5.2 Monitor daemon for query event
```bash
sleep 5
curl -s http://127.0.0.1:8377/events | jq '.[] | select(.type == "query")' | tail -5
```

### 5.3 Wait for response
```bash
sleep 60  # Session.prompt() takes time
tmux capture-pane -t opencode-test:peer-1 -p -S -100 | tail -50
```

**Expected**: Response received from peer-2 via WebSocket.

### 5.4 Verify peer-2 processed the query
```bash
tmux capture-pane -t opencode-test:peer-2 -p -S -100 | tail -30
```

**Expected**: Query appeared in peer-2's chat, response was generated.

## Phase 6: Reverse Query Test

### 6.1 Send query from peer-2 to peer-1
```bash
tmux send-keys -t opencode-test:peer-2 "Use the ask_peer tool to ask $PEER_1_NAME what their main features are" Enter
tmux send-keys -t opencode-test:peer-2 Enter
sleep 60
tmux capture-pane -t opencode-test:peer-2 -p -S -100 | tail -50
```

**Expected**: Response received successfully (bidirectional communication works).

## Phase 7: Status Tracking Test

### 7.1 Send a query and monitor status
```bash
# Start monitoring status in background
watch -n 1 'curl -s http://127.0.0.1:8377/peers | jq ".peers[] | {name, status}"' &
WATCH_PID=$!

# Send query
tmux send-keys -t opencode-test:peer-1 "Use ask_peer to ask $PEER_2_NAME for a detailed explanation of their architecture" Enter
tmux send-keys -t opencode-test:peer-1 Enter

# Let it run for observation
sleep 10
kill $WATCH_PID 2>/dev/null
```

**Expected**:
- Peer-2 status changes to "busy" when processing
- Peer-2 status returns to "online" after completion

### 7.2 Check events for status changes
```bash
curl -s http://127.0.0.1:8377/events | jq '.[] | select(.type == "status_change")'
```

## Phase 8: Validation Summary

### Display Test Results

Before cleanup, display the state for user inspection:

```bash
echo "=== FINAL STATE ==="
echo ""
echo "--- Peers ---"
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status, path}'
echo ""
echo "--- Recent Events ---"
curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
echo ""
echo "--- Peer 1 Last Output ---"
tmux capture-pane -t opencode-test:peer-1 -p -S -30 | tail -20
echo ""
echo "--- Peer 2 Last Output ---"
tmux capture-pane -t opencode-test:peer-2 -p -S -30 | tail -20
```

### Success Criteria
- [ ] Both peers registered via WebSocket (status: online)
- [ ] Peer discovery works (list_peers shows both)
- [ ] Query peer-1 → peer-2: SUCCESS with response
- [ ] Query peer-2 → peer-1: SUCCESS with response
- [ ] Status tracking: Shows "busy" during processing
- [ ] No WebSocket disconnection errors
- [ ] No timeout errors

### Report Results

Present a summary:
```
=== OPENCODE COMMUNICATION TEST RESULTS ===

Plugin Connection:
  peer-1: [PASS/FAIL]
  peer-2: [PASS/FAIL]

Peer Discovery:
  list_peers: [PASS/FAIL]

Bidirectional Queries:
  peer-1 → peer-2: [PASS/FAIL]
  peer-2 → peer-1: [PASS/FAIL]

Status Tracking:
  busy/idle transitions: [PASS/FAIL]

Overall: [PASS/FAIL]
```

## Phase 9: Cleanup

### 9.1 Show final state for user inspection
```bash
echo "Attaching to tmux session for inspection..."
echo "Press Ctrl+B then D to detach when done viewing"
# User can attach with: tmux attach -t opencode-test
```

### 9.2 Kill tmux session (after user confirms)
```bash
tmux kill-session -t opencode-test
```

### 9.3 Prune offline peers
```bash
repowire peer prune --force
```

### 9.4 Stop daemon (optional)
```bash
curl -X POST http://127.0.0.1:8377/shutdown
```

## Troubleshooting

| Issue | Check |
|-------|-------|
| Plugin not connecting | Check daemon running: `curl http://127.0.0.1:8377/health` |
| Peers not registering | Check plugin installed: `ls ~/.config/opencode/plugin/` |
| "No active session" error | User must have an active chat in OpenCode first |
| Query timeout | OpenCode may be slow; increase wait time |
| WebSocket errors | Check daemon logs, restart daemon |
| Wrong peer names | Peer name = folder name where opencode is running |
| Plugin errors | Check opencode logs for [repowire] messages |

## Differences from Claudemux Test

| Aspect | Claudemux | OpenCode |
|--------|-----------|----------|
| Connection | tmux send-keys | WebSocket to daemon |
| Query delivery | tmux paste | SDK session.prompt() |
| Response capture | Stop hook + transcript | Direct SDK response |
| Status tracking | Claude hooks | Plugin events |
| Session setup | `claude` command | `opencode` command |
| Plugin location | ~/.claude/settings.json | ~/.config/opencode/plugin/ |
