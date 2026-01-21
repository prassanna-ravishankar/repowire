---
name: peer-communication-test
description: Integration test for repowire claudemux peer-to-peer messaging. Tests peer registration, inter-agent queries, and mesh network validation end-to-end.
---

# Peer Communication Test

Automated integration test for claudemux backend peer-to-peer communication.

## Execution Mode

Ask user preference:
- **Hands-off**: Execute all steps automatically, report results at end
- **Hands-on**: Confirm each phase before proceeding, show intermediate state

## Phase 1: Environment Discovery

### 1.1 Check existing tmux sessions
```bash
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
```

### 1.2 Check running services
```bash
# Daemon status
curl -s http://127.0.0.1:8377/health 2>/dev/null | jq . || echo "Daemon not running"

# Check for existing repowire processes
pgrep -fl repowire || echo "No repowire processes"
```

### 1.3 Check registered peers
```bash
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | {name, status}' || echo "Cannot fetch peers"
```

### 1.4 Fresh start decision
Ask user: "Found running services. Start fresh (kill existing) or use current state?"

If fresh start:
```bash
# Kill daemon if running
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null
sleep 1

# Kill tmux session if exists
tmux kill-session -t repowire-test 2>/dev/null || true
```

## Phase 2: Project Selection

### 2.1 Check for previously used test projects
Look for recent peer registrations or ask user directly.

### 2.2 Get two project directories
Ask user: "Which two project directories should I use as test peers?"

Requirements:
- Must be valid directories
- Should be git repos (for branch metadata)
- Will run Claude Code sessions in each

Store as `PROJECT_A` and `PROJECT_B`.

## Phase 3: Test Environment Setup

### 3.1 Create tmux session with windows
```bash
# Create session with first window
tmux new-session -d -s repowire-test -n peer-a

# Create second window
tmux new-window -t repowire-test -n peer-b
```

### 3.2 Navigate to project directories
```bash
tmux send-keys -t repowire-test:peer-a "cd $PROJECT_A" Enter
tmux send-keys -t repowire-test:peer-b "cd $PROJECT_B" Enter
```

### 3.3 Start the daemon
```bash
# Start in background, capture output
repowire serve &
sleep 2

# Verify daemon is running
curl -s http://127.0.0.1:8377/health | jq .
```

### 3.4 Verify hooks are installed
```bash
repowire claudemux status
```

If not installed:
```bash
repowire setup --dev --backend claudemux
```

## Phase 4: Launch Claude Sessions

### 4.1 Start Claude in each window
```bash
tmux send-keys -t repowire-test:peer-a "claude" Enter
tmux send-keys -t repowire-test:peer-b "claude" Enter
```

### 4.2 Wait for sessions to initialize
```bash
sleep 5
```

### 4.3 Verify peer registration
```bash
# Both peers should appear with "online" status
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status}'
```

Expected: Two peers with names matching folder names, status "online".

## Phase 5: Communication Test

### 5.1 Send test query
Inject a query into peer-a that requires information from peer-b:

```bash
tmux send-keys -t repowire-test:peer-a "Ask peer '$PEER_B_NAME' what their main purpose or project description is. Use the ask_peer MCP tool." Enter
```

### 5.2 Monitor for response
```bash
# Watch events endpoint for query/response
curl -s http://127.0.0.1:8377/events | jq '.[] | select(.type == "query" or .type == "response")'
```

### 5.3 Verify bidirectional communication (optional)
Send a query from peer-b to peer-a to confirm two-way messaging.

## Phase 6: Validation

### 6.1 Check success criteria
- [ ] Both peers registered and online
- [ ] Query event logged with status "pending" then "success"
- [ ] Response event logged with actual content
- [ ] No timeout errors

### 6.2 Report results
Display:
- Peer registration status
- Query/response timeline from events
- Any errors encountered

### 6.3 Ask for confirmation
"Test completed. Results shown above. Confirm success before teardown?"

## Phase 7: Teardown

Only execute after user confirms test results.

### 7.1 Kill Claude sessions
```bash
tmux kill-window -t repowire-test:peer-a
tmux kill-window -t repowire-test:peer-b
```

### 7.2 Stop daemon
```bash
curl -s -X POST http://127.0.0.1:8377/shutdown
```

### 7.3 Remove tmux session
```bash
tmux kill-session -t repowire-test 2>/dev/null || true
```

### 7.4 Cleanup peers from config (optional)
```bash
repowire peer unregister $PEER_A_NAME
repowire peer unregister $PEER_B_NAME
```

## Troubleshooting

| Issue | Check |
|-------|-------|
| Peers not registering | Verify hooks: `repowire claudemux status` |
| Query timeout | Check daemon logs, verify tmux session names |
| "No tmux session" error | Claude must run inside tmux window |
| Wrong peer names | Peer name = folder name, not window name |
