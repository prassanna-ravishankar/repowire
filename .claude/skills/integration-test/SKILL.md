---
name: integration-test
description: Integration test for repowire peer-to-peer messaging. Supports claudemux, opencode, or mixed-backend testing with circle boundaries and cross-backend communication.
---

# Repowire Integration Test

Unified integration test for peer-to-peer communication across backends.

## Test Modes

| Mode | Description | Use Case |
|------|-------------|----------|
| `claudemux` | Claude Code sessions via tmux hooks | Testing tmux-based communication |
| `opencode` | OpenCode sessions via WebSocket plugin | Testing WebSocket-based communication |
| `mixed` | Both backends together | Testing cross-backend communication |

## Phase 1: Environment Discovery

Run these commands to understand current state before presenting test plan.

### 1.1 System checks
```bash
# Check tmux
tmux list-sessions 2>/dev/null || echo "No tmux sessions"

# Check opencode availability
which opencode 2>/dev/null || echo "opencode not in PATH"

# Check claude availability
which claude 2>/dev/null || echo "claude not in PATH"
```

### 1.2 Daemon and peer status
```bash
curl -s http://127.0.0.1:8377/health 2>/dev/null | jq . || echo "Daemon not running"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | {name, status, circle}' || echo "No peers"
```

### 1.3 Installation status
```bash
# uv tool
uv tool list 2>/dev/null | grep repowire || echo "No uv tool installation"

# Claudemux hooks
cat ~/.claude/settings.json 2>/dev/null | jq '.hooks' || echo "No Claude hooks"

# OpenCode plugin
ls -la ~/.config/opencode/plugin/repowire.ts 2>/dev/null || echo "No OpenCode plugin"
```

## Phase 2: Ask User for Test Configuration

**Present options to user:**

```
=== REPOWIRE INTEGRATION TEST ===

Which test mode do you want to run?

1. claudemux - Test Claude Code sessions (tmux-based)
   - Requires: tmux, claude CLI
   - Tests: Same-circle queries, cross-circle blocking, notifications

2. opencode - Test OpenCode sessions (WebSocket-based)
   - Requires: opencode CLI
   - Tests: WebSocket connection, bidirectional queries, status tracking

3. mixed - Test cross-backend communication
   - Requires: Both tmux/claude AND opencode
   - Tests: Claude Code peer talking to OpenCode peer

Select mode [1/2/3]:
```

**Then ask for project directories based on mode:**

- **claudemux**: 3 directories (2 for circle-a, 1 for circle-b)
- **opencode**: 2 directories
- **mixed**: 2 directories (1 for claude, 1 for opencode)

**Ask about fresh install:**
"Do you want a fresh install? This removes existing hooks/plugins and reinstalls from local dev code."

## Phase 3: Pre-Test Teardown (Always Run)

Clean up any stale state before starting tests. This ensures a consistent baseline.

```bash
# Kill any existing test tmux sessions (safe - only kills known test session names)
tmux kill-session -t circle-a 2>/dev/null || true
tmux kill-session -t circle-b 2>/dev/null || true
tmux kill-session -t opencode-test 2>/dev/null || true
tmux kill-session -t mixed-test 2>/dev/null || true

# Prune stale/offline peers from daemon
curl -s http://127.0.0.1:8377/health >/dev/null 2>&1 && {
  echo "Pruning stale peers..."
  repowire peer prune --force 2>/dev/null || true
}

# Clear any pending query files (claudemux)
rm -f ~/.repowire/pending/*.json 2>/dev/null || true

# Verify clean state
echo "=== Pre-Test State ==="
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers | length' || echo "Daemon not running"
```

## Phase 4: Fresh Install (if requested)

```bash
# Stop daemon first
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null || true
sleep 1

# Uninstall existing installations
repowire uninstall 2>/dev/null || true
uv tool uninstall repowire 2>/dev/null || true

# Remove residual config/state
rm -rf ~/.repowire/pending/ 2>/dev/null || true

# Install (auto-detects and configures all available backends)
repowire setup --dev

# Verify installation
echo "=== Installation Verification ==="
repowire --version
cat ~/.claude/settings.json 2>/dev/null | jq '.hooks | keys' || echo "No Claude hooks"
ls ~/.config/opencode/plugin/repowire.ts 2>/dev/null && echo "OpenCode plugin installed" || echo "No OpenCode plugin"
```

## Phase 5: Test Execution by Mode

---

### Mode: claudemux

#### Setup
```bash
# Ensure daemon running
curl -s http://127.0.0.1:8377/health || (repowire serve &; sleep 2)

# Create tmux sessions (circles)
tmux new-session -d -s circle-a -n peer-a1
tmux new-window -t circle-a -n peer-a2
tmux new-session -d -s circle-b -n peer-b1

# Navigate and start Claude
tmux send-keys -t circle-a:peer-a1 "cd $PROJECT_A1 && claude --dangerously-skip-permissions" Enter
tmux send-keys -t circle-a:peer-a2 "cd $PROJECT_A2 && claude --dangerously-skip-permissions" Enter
tmux send-keys -t circle-b:peer-b1 "cd $PROJECT_B1 && claude --dangerously-skip-permissions" Enter
sleep 10
```

#### Tests
1. **Same-circle query** (peer-a1 → peer-a2): Should succeed
2. **Cross-circle query** (peer-b1 → peer-a1): Should fail with "Circle boundary"
3. **Whoami tool**: Should return peer identity JSON
4. **Notification with correlation ID**: Should embed `[#notif-XXXXXXXX]`

#### Verify
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, circle, status}'
```

#### Cleanup
```bash
tmux kill-session -t circle-a
tmux kill-session -t circle-b
repowire peer prune --force
```

---

### Mode: opencode

#### Setup
```bash
# Start daemon (per-peer routing handles both backends)
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null || true
sleep 1
repowire serve &
sleep 2

# Create tmux session for visual management
tmux new-session -d -s opencode-test -n peer-1
tmux new-window -t opencode-test -n peer-2

# Navigate and start OpenCode
tmux send-keys -t opencode-test:peer-1 "cd $PROJECT_1 && opencode" Enter
tmux send-keys -t opencode-test:peer-2 "cd $PROJECT_2 && opencode" Enter
sleep 5
```

#### Tests
1. **WebSocket connection**: Both peers register via WebSocket
2. **Peer discovery**: list_peers shows both peers
3. **Bidirectional query** (peer-1 → peer-2): Query via session.prompt()
4. **Reverse query** (peer-2 → peer-1): Confirm bidirectional works
5. **Status tracking**: Peer shows "busy" during processing

#### Verify
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online")'
```

#### Cleanup
```bash
tmux kill-session -t opencode-test
repowire peer prune --force
```

---

### Mode: mixed (Cross-Backend)

This tests communication between a Claude Code session (claudemux) and an OpenCode session (opencode).

#### Setup
```bash
# Start daemon with per-peer routing (handles both backends automatically)
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null || true
sleep 1
repowire serve &
sleep 2

# Create tmux sessions
tmux new-session -d -s mixed-test -n claude-peer
tmux new-window -t mixed-test -n opencode-peer

# Start Claude Code peer
tmux send-keys -t mixed-test:claude-peer "cd $PROJECT_CLAUDE && claude --dangerously-skip-permissions" Enter

# Start OpenCode peer
tmux send-keys -t mixed-test:opencode-peer "cd $PROJECT_OPENCODE && opencode" Enter
sleep 10
```

#### Tests

1. **Cross-backend peer discovery**
   Both peers should see each other in list_peers despite different backends.
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status}'
   ```

2. **Claude → OpenCode query**
   From Claude Code peer, query the OpenCode peer:
   ```bash
   tmux send-keys -t mixed-test:claude-peer "Use ask_peer to ask $OPENCODE_PEER_NAME what their project is about" Enter Enter
   sleep 60  # Adjust based on model response time; check daemon /events for completion
   tmux capture-pane -t mixed-test:claude-peer -p -S -50 | tail -30
   ```
   Expected: Response received from OpenCode peer.

3. **OpenCode → Claude query**
   From OpenCode peer, query the Claude Code peer:
   ```bash
   tmux send-keys -t mixed-test:opencode-peer "Use the ask_peer tool to ask $CLAUDE_PEER_NAME about their architecture" Enter
   sleep 60  # Adjust based on model response time; check daemon /events for completion
   tmux capture-pane -t mixed-test:opencode-peer -p -S -50 | tail -30
   ```
   Expected: Response received from Claude Code peer.

4. **Circle join for cross-backend**
   OpenCode peers default to "global" circle. Use set_circle to join the tmux session's circle:
   ```bash
   tmux send-keys -t mixed-test:opencode-peer "Use the set_circle tool to join circle 'mixed-test'" Enter
   sleep 5
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, circle}'
   ```

#### Verify
```bash
echo "=== Cross-Backend Test Results ==="
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status, circle}'
curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
```

#### Cleanup
```bash
tmux kill-session -t mixed-test
repowire peer prune --force
```

---

## Phase 6: Validation Summary

### Success Criteria by Mode

#### claudemux
- [ ] All peers registered with correct circles
- [ ] Same-circle query: SUCCESS
- [ ] Cross-circle query: BLOCKED with error
- [ ] Whoami returns correct identity
- [ ] Notifications include correlation ID

#### opencode
- [ ] Both peers connected via WebSocket
- [ ] list_peers shows both peers
- [ ] Bidirectional queries work
- [ ] Status tracking (busy/idle) works

#### mixed
- [ ] Both backend types register successfully
- [ ] Claude → OpenCode query: SUCCESS
- [ ] OpenCode → Claude query: SUCCESS
- [ ] Circle join works across backends

### Report Template

```
=== REPOWIRE INTEGRATION TEST RESULTS ===

Mode: [claudemux/opencode/mixed]

Peer Registration:
  [peer-name]: [PASS/FAIL] (backend: [claudemux/opencode])

Communication Tests:
  [test-name]: [PASS/FAIL]

Overall: [PASS/FAIL]
```

## Phase 7: Final Teardown

Always run after tests complete (success or failure) to restore clean state.

### 7.1 Kill Test Sessions
```bash
# Kill all test-related tmux sessions
tmux kill-session -t circle-a 2>/dev/null || true
tmux kill-session -t circle-b 2>/dev/null || true
tmux kill-session -t opencode-test 2>/dev/null || true
tmux kill-session -t mixed-test 2>/dev/null || true

echo "Test sessions terminated"
```

### 7.2 Clean Up Peers
```bash
# Mark all test peers offline and prune
repowire peer prune --force 2>/dev/null || true

# Verify no stale peers remain
curl -s http://127.0.0.1:8377/peers | jq '.peers | length' || echo "Daemon not running"
```

### 7.3 Stop Daemon (Optional)
Only stop if you don't need it running for other work.

```bash
# Stop daemon
curl -s -X POST http://127.0.0.1:8377/shutdown 2>/dev/null && echo "Daemon stopped" || echo "Daemon was not running"
```

### 7.4 Clean Up State Files (Optional - for full reset)
Only run if you want to completely reset repowire state.

```bash
# Remove pending queries
rm -rf ~/.repowire/pending/ 2>/dev/null || true

# Remove peer config (CAUTION: removes all peer definitions)
# rm ~/.repowire/config.yaml 2>/dev/null || true

echo "State files cleaned"
```

### 7.5 Final State Verification
```bash
echo "=== Final State ==="
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
curl -s http://127.0.0.1:8377/health 2>/dev/null && echo "Daemon: running" || echo "Daemon: stopped"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | .name' || echo "No peers registered"
```

## Troubleshooting

| Issue | Check |
|-------|-------|
| Daemon not running | `curl http://127.0.0.1:8377/health` |
| Claudemux peers not registering | `repowire claudemux status` |
| OpenCode plugin not connecting | `ls ~/.config/opencode/plugin/` |
| Wrong peer names | Peer name = folder name |
| Circle mismatch | Claudemux circle = tmux session name |
| Cross-backend fails | Check both backends installed, use set_circle |
| Query timeout | Increase sleep time, check daemon logs |

## Quick Reference

```bash
# Start daemon (per-peer routing)
repowire serve

# Check peers
curl -s http://127.0.0.1:8377/peers | jq '.peers[]'

# Check events
curl -s http://127.0.0.1:8377/events | jq '.[-5:]'

# Prune offline peers
repowire peer prune --force

# Stop daemon
curl -X POST http://127.0.0.1:8377/shutdown
```
