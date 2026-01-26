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

Use `repowire peer ask` CLI for reliable query testing (bypasses tmux UI issues).

**Direct query** tests CLI→daemon→peer flow:
```bash
uv run repowire peer ask $PEER_NAME "question" -t 120
```

**Proxy query** tests full peer-to-peer flow (peer-a asks peer-b via MCP tools):
```bash
uv run repowire peer ask $PEER_A_NAME "Use ask_peer to ask $PEER_B_NAME: question" -t 180
```

#### Tests

1. **Direct query to peer-a1**
   ```bash
   uv run repowire peer ask $PEER_A1_NAME "What is this project about in one sentence?" -t 120
   ```
   Expected: Direct response from peer-a1

2. **Peer-to-peer query via proxy** (peer-a1 → peer-a2)
   ```bash
   # Ask peer-a1 to use its ask_peer MCP tool to query peer-a2
   uv run repowire peer ask $PEER_A1_NAME \
     "Use the ask_peer tool to ask $PEER_A2_NAME: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```
   Expected: peer-a1 responds with peer-a2's answer (tests full mesh communication)

3. **Verify peers are in same circle**
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.circle == "circle-a") | {name, status, circle, pane_id}'
   ```

4. **Verify pane_id was captured correctly**
   ```bash
   # pane_id should be tmux pane format like "%22", not "legacy:..."
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, pane_id}'
   ```

5. **Check events for query chain**
   ```bash
   curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
   ```

#### Verify
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, circle, status, pane_id}'
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
curl -s http://127.0.0.1:8377/health || (uv run repowire serve &; sleep 2)

# Create tmux session for OpenCode peers (OpenCode runs inside tmux for pane_id)
tmux new-session -d -s opencode-test -n peer-1 -c $PROJECT_1
tmux new-window -t opencode-test -n peer-2 -c $PROJECT_2

# Start OpenCode in each window
tmux send-keys -t opencode-test:peer-1 "opencode" Enter
tmux send-keys -t opencode-test:peer-2 "opencode" Enter
sleep 10
```

#### Tests

Use `repowire peer ask` CLI with proxy pattern for reliable testing.

1. **WebSocket connection & peer discovery**
   ```bash
   # Both peers should register via WebSocket with real pane_id
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.backend == "opencode") | {name, status, pane_id}'
   ```
   Expected: Both peers online with pane_id like "%XX"

2. **Direct query to peer-1**
   ```bash
   uv run repowire peer ask $PEER_1_NAME "What is this project about in one sentence?" -t 120
   ```
   Expected: Direct response from peer-1

3. **Peer-to-peer query via proxy** (peer-1 → peer-2)
   ```bash
   uv run repowire peer ask $PEER_1_NAME \
     "Use the ask_peer tool to ask $PEER_2_NAME: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```
   Expected: peer-1 responds with peer-2's answer

4. **Reverse peer-to-peer query** (peer-2 → peer-1)
   ```bash
   uv run repowire peer ask $PEER_2_NAME \
     "Use the ask_peer tool to ask $PEER_1_NAME: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```
   Expected: peer-2 responds with peer-1's answer (confirms bidirectional)

5. **Verify pane_id captured correctly**
   ```bash
   # Both should have real pane_id, not "legacy:..." or "opencode:..."
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, backend, pane_id}'
   ```

6. **Check events for query chain**
   ```bash
   curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
   ```

#### Verify
```bash
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.backend == "opencode") | {name, status, pane_id}'
```

#### Cleanup
```bash
tmux kill-session -t opencode-test
uv run repowire peer prune --force
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

Use `repowire peer ask` CLI with proxy pattern for reliable cross-backend testing.

1. **Cross-backend peer discovery**
   Both peers should see each other in list_peers despite different backends.
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status, backend, pane_id}'
   ```

2. **Claude → OpenCode query (via proxy)**
   Ask Claude peer to query the OpenCode peer using its MCP tools:
   ```bash
   # This sends query to claude-peer, which then uses ask_peer to query opencode-peer
   uv run repowire peer ask $CLAUDE_PEER_NAME \
     "Use the ask_peer tool to ask $OPENCODE_PEER_NAME: What is this project about in one sentence? Return their response." \
     -t 180
   ```
   Expected: Claude peer responds with OpenCode peer's answer.

3. **OpenCode → Claude query (via proxy)**
   Ask OpenCode peer to query the Claude peer using its MCP tools:
   ```bash
   uv run repowire peer ask $OPENCODE_PEER_NAME \
     "Use the ask_peer tool to ask $CLAUDE_PEER_NAME: What is this project about in one sentence? Return their response." \
     -t 180
   ```
   Expected: OpenCode peer responds with Claude peer's answer.

4. **Verify both backends registered with pane_id**
   ```bash
   # Both should have real pane_id (e.g., "%22"), not "legacy:..."
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, backend, pane_id}'
   ```

5. **Check events for full query chain**
   ```bash
   # Should show: CLI→peer-a (query), peer-a→peer-b (query), peer-b→peer-a (response), peer-a→CLI (response)
   curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
   ```

#### Verify
```bash
echo "=== Cross-Backend Test Results ==="
curl -s http://127.0.0.1:8377/peers | jq '.peers[] | {name, status, backend, circle, pane_id}'
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
- [ ] All peers registered with real pane_id (e.g., "%22", not "legacy:...")
- [ ] Peers in correct circles (circle = tmux session name)
- [ ] Direct query via CLI: SUCCESS
- [ ] Peer-to-peer query via proxy: SUCCESS (tests full mesh)

#### opencode
- [ ] Both peers connected via WebSocket with real pane_id
- [ ] list_peers shows both peers as "opencode" backend
- [ ] Direct query via CLI: SUCCESS
- [ ] Bidirectional peer-to-peer queries via proxy: SUCCESS

#### mixed
- [ ] Both backend types register with real pane_id
- [ ] Claude → OpenCode query via proxy: SUCCESS
- [ ] OpenCode → Claude query via proxy: SUCCESS
- [ ] Events show full query chain across backends

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
