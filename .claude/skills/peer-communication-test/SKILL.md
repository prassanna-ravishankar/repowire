---
name: peer-communication-test
description: Integration test for repowire claudemux peer-to-peer messaging. Tests peer registration, circle boundaries, inter-agent queries, and mesh network validation end-to-end.
---

# Peer Communication Test

Integration test for claudemux backend peer-to-peer communication with circle boundary enforcement.

## Prerequisites

Suggest using `claude --dangerously-skip-permissions` for test sessions to avoid permission prompts during automated testing.

## Phase 1: Environment Discovery

Gather current state before presenting test plan.

### 1.1 Check existing tmux sessions
```bash
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
```

### 1.2 Check daemon and peers
```bash
curl -s http://127.0.0.1:8377/health 2>/dev/null | jq . || echo "Daemon not running"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | {name, status, circle}' || echo "No peers"
```

## Phase 2: Present Test Plan for Confirmation

**Before executing any test steps**, present the full plan to the user:

### Test Plan Template

```
=== REPOWIRE PEER COMMUNICATION TEST PLAN ===

TMUX SESSIONS TO CREATE:
  • circle-a (for same-circle test)
  • circle-b (for cross-circle test)

TEST PROJECTS (ask user for 3 directories):
  Circle A:
    • peer-a1: <PROJECT_A1>
    • peer-a2: <PROJECT_A2>
  Circle B:
    • peer-b1: <PROJECT_B1>

TESTS TO RUN:
  1. Same-circle communication (peer-a1 → peer-a2)
     Expected: SUCCESS

  2. Cross-circle communication (peer-b1 → peer-a1)
     Expected: BLOCKED with circle boundary error

  3. whoami tool (peer-a1)
     Expected: Returns peer identity JSON

COMMANDS TO EXECUTE:
  # Create sessions
  tmux new-session -d -s circle-a -n peer-a1
  tmux new-window -t circle-a -n peer-a2
  tmux new-session -d -s circle-b -n peer-b1

  # Start Claude (with --dangerously-skip-permissions)
  tmux send-keys -t circle-a:peer-a1 "cd <PROJECT_A1> && claude --dangerously-skip-permissions" Enter
  tmux send-keys -t circle-a:peer-a2 "cd <PROJECT_A2> && claude --dangerously-skip-permissions" Enter
  tmux send-keys -t circle-b:peer-b1 "cd <PROJECT_B1> && claude --dangerously-skip-permissions" Enter

CLEANUP:
  tmux kill-session -t circle-a
  tmux kill-session -t circle-b
  repowire peer prune --force

Proceed with test? [y/N]
```

**Wait for user confirmation before proceeding.**

## Phase 3: Test Environment Setup

### 3.1 Ensure daemon is running
```bash
curl -s http://127.0.0.1:8377/health | jq . || repowire serve &
sleep 2
```

### 3.2 Verify hooks are installed
```bash
repowire claudemux status
```

If not installed:
```bash
repowire setup --dev --backend claudemux
```

### 3.3 Create tmux sessions (circles)
```bash
# Circle A - two peers for same-circle test
tmux new-session -d -s circle-a -n peer-a1
tmux new-window -t circle-a -n peer-a2

# Circle B - one peer for cross-circle test
tmux new-session -d -s circle-b -n peer-b1
```

### 3.4 Navigate to projects
```bash
tmux send-keys -t circle-a:peer-a1 "cd $PROJECT_A1" Enter
tmux send-keys -t circle-a:peer-a2 "cd $PROJECT_A2" Enter
tmux send-keys -t circle-b:peer-b1 "cd $PROJECT_B1" Enter
```

### 3.5 Start Claude sessions
```bash
tmux send-keys -t circle-a:peer-a1 "claude --dangerously-skip-permissions" Enter
tmux send-keys -t circle-a:peer-a2 "claude --dangerously-skip-permissions" Enter
tmux send-keys -t circle-b:peer-b1 "claude --dangerously-skip-permissions" Enter
sleep 10
```

### 3.6 Verify peer registration
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, circle}'
```

Expected:
- `$PEER_A1_NAME` in circle `circle-a`
- `$PEER_A2_NAME` in circle `circle-a`
- `$PEER_B1_NAME` in circle `circle-b`

## Phase 4: Same-Circle Communication Test

### 4.1 Send query from peer-a1 to peer-a2
```bash
tmux send-keys -t circle-a:peer-a1 "Use the ask_peer MCP tool to ask $PEER_A2_NAME what their project is about" Enter Enter
```

### 4.2 Wait and verify
```bash
sleep 45
tmux capture-pane -t circle-a:peer-a1 -p -S -100 | tail -50
```

**Expected**: Response received successfully (both in same circle).

## Phase 5: Cross-Circle Communication Test

### 5.1 Send query from peer-b1 to peer-a1 (different circles)
```bash
tmux send-keys -t circle-b:peer-b1 "Use the ask_peer MCP tool to ask $PEER_A1_NAME what their project is about" Enter Enter
```

### 5.2 Wait and verify
```bash
sleep 45
tmux capture-pane -t circle-b:peer-b1 -p -S -100 | tail -50
```

**Expected**: Error message containing "Circle boundary" - communication blocked.

## Phase 6: Whoami Tool Test

### 6.1 Test whoami
```bash
tmux send-keys -t circle-a:peer-a1 "Use the whoami MCP tool" Enter Enter
```

### 6.2 Verify response
```bash
sleep 30
tmux capture-pane -t circle-a:peer-a1 -p -S -50 | tail -30
```

**Expected**: JSON with name, circle, status, path, machine, metadata.

## Phase 7: Notification with Correlation Tracking

### 7.1 Send notification from peer-a1 to peer-a2
```bash
tmux send-keys -t circle-a:peer-a1 "Send a notification to peer '$PEER_A2_NAME' saying 'Build completed successfully'. Use the notify_peer MCP tool and note the correlation ID returned." Enter
```

### 7.2 Verify correlation ID format
Check events endpoint for notification with embedded correlation ID:

```bash
curl -s http://127.0.0.1:8377/events | jq '.[] | select(.type == "notification")'
```

Expected: Message text contains `[#notif-XXXXXXXX]` prefix.

### 7.3 Verify peer-a2 received the notification
The notification should appear in peer-a2's session with the correlation ID embedded, allowing peer-a2 to reference it in any follow-up communication.

## Phase 8: Validation Summary

### Success Criteria
- [ ] All peers registered with correct circles
- [ ] Same-circle query (peer-a1 → peer-a2): SUCCESS
- [ ] Cross-circle query (peer-b1 → peer-a1): BLOCKED
- [ ] Whoami returns correct peer identity
- [ ] No timeout errors
- [ ] Notification event logged with correlation ID in message
- [ ] Correlation ID format matches `notif-XXXXXXXX`

### Report Results
Display:
- Peer registration status with circles
- Same-circle test result
- Cross-circle test result (should show boundary error)
- Whoami output

## Phase 9: Teardown

### 9.1 Kill tmux sessions
```bash
tmux kill-session -t circle-a
tmux kill-session -t circle-b
```

### 9.2 Prune offline peers
```bash
repowire peer prune --force
```

## Troubleshooting

| Issue | Check |
|-------|-------|
| Peers not registering | Verify hooks: `repowire claudemux status` |
| Query timeout | Check daemon logs, verify tmux session names |
| "No tmux session" error | Claude must run inside tmux window |
| Wrong peer names | Peer name = folder name, not window name |
| Wrong circle | Circle = tmux session name |
| Circle boundary not working | Check MCP config has no `--directory` flag |
| Enter not working | Press Enter twice, check status goes to "busy" |
