---
name: integration-test
description: Integration test for repowire peer-to-peer messaging. Supports claude-code, opencode, or mixed-agent-type testing with circle boundaries and cross-agent-type communication. Can run all modes in parallel via agent teams.
---

# Repowire Integration Test

Unified integration test for peer-to-peer communication across agent types.

## Execution Modes

| Argument | Behavior |
|----------|----------|
| `claude-code` | Test Claude Code agent type only |
| `opencode` | Test OpenCode agent type only |
| `mixed` | Test cross-agent-type communication |
| `all` or no argument | Run all three modes in parallel using agent teams |

## Team-Based Parallel Execution

When `all` is specified (or no argument), the lead agent should:

1. Run Phase 1 (environment discovery) and Phase 3 (pre-test teardown) itself
2. Ask user for project directories and opencode model (Phase 2)
3. Ensure daemon is running
4. Create tasks for each mode and spawn background agents in parallel:
   - Agent `test-claude-code` → runs claude-code tests
   - Agent `test-opencode` → runs opencode tests
   - Agent `test-mixed` → runs mixed tests (depends on both agent types being available)
5. Collect results and produce unified report

### Spawning agents

Use the Task tool with `run_in_background: true` to run modes in parallel:

```
Task(subagent_type="general-purpose", run_in_background=true, prompt="Run the claude-code integration test. [paste mode section]")
Task(subagent_type="general-purpose", run_in_background=true, prompt="Run the opencode integration test. [paste mode section]")
```

Each agent gets the full context for its mode and runs independently.

## Phase 1: Environment Discovery

Run these commands to understand current state. Keep all phases in the same
shell so `REPOWIRE_BIN` continues to identify the exact binary under test.

```bash
# Pin every command in this workflow to one binary. Phase 4 replaces this with
# the freshly built checkout binary when a fresh install is requested.
REPOWIRE_BIN="$(command -v repowire || true)"
export REPOWIRE_BIN

# System checks
tmux list-sessions 2>/dev/null || echo "No tmux sessions"
which opencode 2>/dev/null || echo "opencode not in PATH"
which claude 2>/dev/null || echo "claude not in PATH"

# Daemon and peer status
curl -s http://127.0.0.1:8377/health 2>/dev/null | jq . || echo "Daemon not running"
curl -s http://127.0.0.1:8377/peers 2>/dev/null | jq '.peers[] | {name, status, circle}' || echo "No peers"

# Installation status
test -n "$REPOWIRE_BIN" && "$REPOWIRE_BIN" --version || echo "Repowire not installed"
jq -r '"Hooks: " + ((.hooks // {}) | keys | join(", "))' "$HOME/.claude/settings.json" 2>/dev/null || echo "No Claude hooks"
ls ~/.opencode/plugin/repowire.ts 2>/dev/null && echo "OpenCode plugin installed" || echo "No OpenCode plugin"
```

## Phase 2: Ask User for Configuration

Ask user for:
1. **Project directories** (2-3 depending on mode, or ask for all if running `all`)
2. **OpenCode model** (e.g., `anthropic/claude-sonnet-4-5-20250929`) — needed for opencode/mixed modes
3. **Fresh install?** — removes existing hooks/plugins and reinstalls

## Phase 3: Pre-Test Teardown (Always Run)

```bash
tmux kill-session -t circle-a 2>/dev/null || true
tmux kill-session -t circle-b 2>/dev/null || true
tmux kill-session -t opencode-test 2>/dev/null || true
tmux kill-session -t mixed-test 2>/dev/null || true
curl -s http://127.0.0.1:8377/health >/dev/null 2>&1 && {
  "$REPOWIRE_BIN" peer prune --force 2>/dev/null || true
}

rm -f ~/.repowire/pending/*.json 2>/dev/null || true
rm -f ~/.repowire/sessions.json 2>/dev/null || true
```

## Phase 4: Fresh Install (if requested)

**IMPORTANT:** `repowire setup` calls `claude mcp add` which fails inside a Claude Code session. Run these commands in a **tmux pane**, not from within Claude Code.

```bash
# Run in a tmux pane (e.g. tmux send-keys to a spare window):

# Build from local source
cd /path/to/repowire
mkdir -p bin
(cd daemon-go && go build -o ../bin/repowire .)
REPOWIRE_BIN="$(pwd)/bin/repowire"
export REPOWIRE_BIN

# Setup hooks and MCP server
"$REPOWIRE_BIN" setup --no-service

# Verify
"$REPOWIRE_BIN" --version
jq -r '"Hooks: " + ((.hooks // {}) | keys | join(", "))' "$HOME/.claude/settings.json"
ls ~/.opencode/plugin/repowire.ts 2>/dev/null && echo "OpenCode plugin: OK"
```

## Phase 5: Ensure Daemon Running

```bash
test -x "$REPOWIRE_BIN" || { echo "REPOWIRE_BIN is not executable: $REPOWIRE_BIN" >&2; exit 1; }
curl -s http://127.0.0.1:8377/health || {
  nohup "$REPOWIRE_BIN" serve > /tmp/repowire-daemon.log 2>&1 &
  sleep 3
}
curl -s http://127.0.0.1:8377/health | jq .
```

---

## Mode: claude-code

**Requires:** tmux, claude CLI, Claude hooks installed
**Tmux sessions used:** `circle-a`, `circle-b`
**Project directories needed:** 3 (2 for circle-a, 1 for circle-b)

### Setup

```bash
tmux new-session -d -s circle-a -n peer-a1
tmux new-window -t circle-a -n peer-a2
tmux new-session -d -s circle-b -n peer-b1

tmux send-keys -t circle-a:peer-a1 "cd $PROJECT_A1 && claude --dangerously-skip-permissions" Enter
sleep 3
tmux send-keys -t circle-a:peer-a2 "cd $PROJECT_A2 && claude --dangerously-skip-permissions" Enter
sleep 3
tmux send-keys -t circle-b:peer-b1 "cd $PROJECT_B1 && claude --dangerously-skip-permissions" Enter
sleep 30  # Wait for sessions to initialize and hooks to register
```

### Verify Registration

```bash
# All 3 peers should be online with correct circles and peer_id format
curl -fsS http://127.0.0.1:8377/peers | jq -e --arg a1 "$PEER_A1" --arg a2 "$PEER_A2" --arg b1 "$PEER_B1" '
  def valid($name; $circle):
    [.peers[] | select(
      .name == $name and
      .status == "online" and
      .circle == $circle and
      (.peer_id | test("^repow-" + $circle + "-[a-f0-9]{8}$"))
    )] | length == 1;
  if valid($a1; "circle-a") and valid($a2; "circle-a") and valid($b1; "circle-b") then
    [.peers[] | select(.name == $a1 or .name == $a2 or .name == $b1) |
     {name, status, circle, peer_id}]
  else
    error("registration assertion failed: expected exactly one online peer for each name with the correct circle and peer_id")
  end
'
```

If a peer doesn't register, check `.claude/settings.local.json` for `disableAllHooks: true`.

### Tests

1. **Direct query to peer-a1**
   ```bash
   "$REPOWIRE_BIN" peer ask $PEER_A1 "What is this project about in one sentence?" -t 120
   ```
   Expected: Direct response describing the project.

2. **Peer-to-peer proxy** (peer-a1 → peer-a2 via MCP ask_peer)
   ```bash
   "$REPOWIRE_BIN" peer ask $PEER_A1 \
     "Use the ask_peer tool to ask $PEER_A2: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```
   Expected: peer-a1 responds with peer-a2's answer.

3. **Circle verification**
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.circle == "circle-a") | {name, peer_id}'
   ```

4. **Event chain**
   ```bash
   curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
   ```

### Cleanup

```bash
tmux kill-session -t circle-a 2>/dev/null || true
tmux kill-session -t circle-b 2>/dev/null || true
"$REPOWIRE_BIN" peer prune --force 2>/dev/null || true
```

### Success Criteria

- [ ] All 3 peers registered with peer_id format `repow-{circle}-{uuid8}`
- [ ] Peers in correct circles (circle = tmux session name)
- [ ] Direct query via CLI: PASS
- [ ] Peer-to-peer proxy query via MCP: PASS

---

## Mode: opencode

**Requires:** opencode CLI, OpenCode plugin installed at `~/.opencode/plugin/repowire.ts`
**Tmux sessions used:** `opencode-test`
**Project directories needed:** 2
**Model flag needed:** e.g., `--model anthropic/claude-sonnet-4-5-20250929`

### Important Notes

- OpenCode does NOT accept tmux send-keys for prompt submission (alternate screen mode). All query injection goes through the SDK plugin via `session.prompt()`.
- OpenCode sessions need a warmup prompt (via tmux send-keys) to create an active session before repowire queries work.
- The `--model` flag must specify a working model. LiteLLM proxy models may return 404.

### Setup

```bash
tmux new-session -d -s opencode-test -n peer-1
tmux new-window -t opencode-test -n peer-2

tmux send-keys -t opencode-test:peer-1 "cd $PROJECT_1 && opencode --model $OPENCODE_MODEL" Enter
sleep 3
tmux send-keys -t opencode-test:peer-2 "cd $PROJECT_2 && opencode --model $OPENCODE_MODEL" Enter
sleep 10

# Warm up: send initial prompt to create sessions
tmux send-keys -t opencode-test:peer-1 -l "hi"
tmux send-keys -t opencode-test:peer-1 Enter
sleep 2
tmux send-keys -t opencode-test:peer-2 -l "hi"
tmux send-keys -t opencode-test:peer-2 Enter
sleep 30  # Wait for warmup to complete
```

### Verify Registration

```bash
curl -s http://127.0.0.1:8377/peers | jq '
  [.peers[] | select(.backend == "opencode" and .status == "online") |
   {name, peer_id, backend}]
'
```

### Tests

1. **WebSocket connection & peer discovery**
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.backend == "opencode") | {name, status, peer_id}'
   ```
   Expected: Both peers online with `repow-{circle}-{uuid8}` format.

2. **Direct query to peer-1**
   ```bash
   "$REPOWIRE_BIN" peer ask $PEER_1 "What is this project about in one sentence?" -t 120
   ```
   Expected: Response from the model (not "empty response" or "Not Found").

3. **Peer-to-peer proxy** (peer-1 → peer-2)
   ```bash
   "$REPOWIRE_BIN" peer ask $PEER_1 \
     "Use the ask_peer tool to ask $PEER_2: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```

4. **Reverse proxy** (peer-2 → peer-1)
   ```bash
   "$REPOWIRE_BIN" peer ask $PEER_2 \
     "Use the ask_peer tool to ask $PEER_1: What is this project about in one sentence? Return their exact response." \
     -t 180
   ```

### Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| "No active session" | Warmup prompt didn't create session | Press Enter manually in opencode pane, or wait longer |
| "(empty response)" | Model returned 0 parts | Check model config — switch to `anthropic/claude-sonnet-4-5-20250929` |
| "Not Found" in TUI | LiteLLM model ID not recognized | Use `--model anthropic/<model-id>` directly |
| Plugin not connecting | Wrong WS URL or old plugin | Reinstall with `"$REPOWIRE_BIN" setup --no-service` |

### Cleanup

```bash
tmux kill-session -t opencode-test 2>/dev/null || true
"$REPOWIRE_BIN" peer prune --force 2>/dev/null || true
```

### Success Criteria

- [ ] Both peers registered via WebSocket with `repow-{circle}-{uuid8}` peer_id
- [ ] Direct query returns real model response (not empty/error)
- [ ] Bidirectional peer-to-peer proxy queries: PASS

---

## Mode: mixed (Cross-Agent-Type)

**Requires:** Both claude-code AND opencode setups working
**Tmux sessions used:** `mixed-test`
**Project directories needed:** 2 (1 for claude, 1 for opencode)
**Depends on:** claude-code and opencode modes passing individually first

### Setup

```bash
tmux new-session -d -s mixed-test -n claude-peer
tmux new-window -t mixed-test -n opencode-peer

tmux send-keys -t mixed-test:claude-peer "cd $PROJECT_CLAUDE && claude --dangerously-skip-permissions" Enter
sleep 3
tmux send-keys -t mixed-test:opencode-peer "cd $PROJECT_OPENCODE && opencode --model $OPENCODE_MODEL" Enter
sleep 10

# Warm up opencode peer
tmux send-keys -t mixed-test:opencode-peer -l "hi"
tmux send-keys -t mixed-test:opencode-peer Enter
sleep 30
```

### Tests

1. **Cross-agent-type registration**
   ```bash
   curl -s http://127.0.0.1:8377/peers | jq '.peers[] | select(.status == "online") | {name, backend, peer_id}'
   ```

2. **Claude → OpenCode proxy**
   ```bash
   "$REPOWIRE_BIN" peer ask $CLAUDE_PEER \
     "Use ask_peer to ask $OPENCODE_PEER: What is this project about in one sentence? Return their response." \
     -t 180
   ```

3. **OpenCode → Claude proxy**
   ```bash
   "$REPOWIRE_BIN" peer ask $OPENCODE_PEER \
     "Use ask_peer to ask $CLAUDE_PEER: What is this project about in one sentence? Return their response." \
     -t 180
   ```

4. **Event chain**
   ```bash
   curl -s http://127.0.0.1:8377/events | jq '.[-10:]'
   ```

### Cleanup

```bash
tmux kill-session -t mixed-test 2>/dev/null || true
"$REPOWIRE_BIN" peer prune --force 2>/dev/null || true
```

### Success Criteria

- [ ] Both agent types register with `repow-{circle}-{uuid8}` peer_id
- [ ] Claude → OpenCode proxy: PASS
- [ ] OpenCode → Claude proxy: PASS

---

## Phase 6: Unified Report

Aggregate results from all modes into a single report:

```
==========================================
  REPOWIRE INTEGRATION TEST RESULTS
==========================================

  MODE: CLAUDE-CODE
  ├ Registration:    PASS/FAIL (X/Y peers)
  ├ Direct query:    PASS/FAIL
  ├ Proxy query:     PASS/FAIL
  └ Status:          PASS/FAIL

  MODE: OPENCODE
  ├ Registration:    PASS/FAIL (X/Y peers)
  ├ Direct query:    PASS/FAIL
  ├ Proxy query:     PASS/FAIL
  └ Status:          PASS/FAIL

  MODE: MIXED
  ├ Registration:    PASS/FAIL
  ├ Claude→OC:       PASS/FAIL
  ├ OC→Claude:       PASS/FAIL
  └ Status:          PASS/FAIL

  Overall: PASS/FAIL
==========================================
```

## Phase 7: Final Teardown

```bash
tmux kill-session -t circle-a 2>/dev/null || true
tmux kill-session -t circle-b 2>/dev/null || true
tmux kill-session -t opencode-test 2>/dev/null || true
tmux kill-session -t mixed-test 2>/dev/null || true
"$REPOWIRE_BIN" peer prune --force 2>/dev/null || true
```

## Quick Reference

```bash
"$REPOWIRE_BIN" serve                    # Start daemon
"$REPOWIRE_BIN" peer ask NAME "q" -t 120 # Query a peer
curl -s localhost:8377/peers | jq  # List peers
curl -s localhost:8377/events | jq '.[-5:]'  # Recent events
"$REPOWIRE_BIN" peer prune --force       # Remove offline peers
```
