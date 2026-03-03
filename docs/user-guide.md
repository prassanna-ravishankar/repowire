# Repowire Setup & Data-ML-Platform Team Guide

## 1. Install Repowire

You already have it cloned at `/Users/ahmad.ragab/Dev/tools/repowire`. Install in dev mode:

```bash
cd /Users/ahmad.ragab/Dev/tools/repowire
pip install -e "."
# or with uv:
uv pip install -e "."
```

Verify:

```bash
repowire --help
```

## 2. Run Setup (Both Backends)

```bash
repowire setup --dev
```

This auto-detects and configures:
- **Claudemux** (if `claude` CLI found): Installs 5 hooks into `~/.claude/settings.json` + registers MCP server
- **OpenCode** (if `opencode` CLI or `~/.config/opencode` found): Installs WebSocket plugin
- **Daemon service**: Installs as launchd service (`~/Library/LaunchAgents/io.repowire.daemon.plist`)

The `--dev` flag makes hooks use `uv run --directory /Users/ahmad.ragab/Dev/tools/repowire repowire` so your local source changes take effect immediately.

Verify setup:

```bash
repowire status
```

You should see checkmarks for backends and daemon.

## 3. Start the Daemon

If the service installed correctly, it's already running. If not:

```bash
# Foreground (for debugging):
repowire serve

# Or install service manually:
repowire service install
```

Verify it's alive:

```bash
curl http://127.0.0.1:8377/health
```

## 4. Spawn the Data-ML-Platform Team

Create all 8 peers in a `data-ml-platform` circle. Each command opens a tmux window with Claude Code (or OpenCode) in the repo directory:

```bash
# Core infrastructure
repowire peer new /Users/ahmad.ragab/Dev/handshake/terraform --circle data-ml-platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/ops --circle data-ml-platform

# Data platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/data-platform --circle data-ml-platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/dataflow-pipelines --circle data-ml-platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/handshake-data-python-libs --circle data-ml-platform

# ML platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/core-ml-platform --circle data-ml-platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/llm-orchestration --circle data-ml-platform
repowire peer new /Users/ahmad.ragab/Dev/handshake/handshake-agent-platform --circle data-ml-platform
```

Each peer's name is auto-derived from the folder name (e.g., `terraform`, `ops`, `data-platform`).

**To use OpenCode for specific peers instead**, add `--backend opencode`:

```bash
repowire peer new /Users/ahmad.ragab/Dev/handshake/llm-orchestration --circle data-ml-platform --backend opencode
```

**To pass custom Claude flags** (e.g., skip permissions for autonomous work):

```bash
repowire peer new /Users/ahmad.ragab/Dev/handshake/terraform --circle data-ml-platform --command "claude --dangerously-skip-permissions"
```

## 5. Verify Peers Are Running

```bash
repowire peer list
```

Expected output: 8 peers, all with green status, in the `data-ml-platform` circle.

Or check from the TUI:

```bash
repowire top
```

This opens an htop-style dashboard. Peers are grouped under the `data-ml-platform` circle in the left pane.

## 6. Attach to a Peer Session

From the TUI, press `s` on a peer to attach to its tmux session.

Or directly via tmux:

```bash
# Circle name = tmux session name
tmux attach-session -t data-ml-platform
```

Navigate windows with `Ctrl-b n` (next) / `Ctrl-b p` (previous) to switch between repo sessions.

## 7. Inter-Peer Communication

Once inside any Claude Code session in the circle, the MCP tools are available automatically:

```
# From any peer, list who's online:
list_peers()

# Ask the terraform peer a question (synchronous, waits for response):
ask_peer("terraform", "What GCP projects are defined and what services do they host?")

# Send a notification (fire-and-forget):
notify_peer("data-platform", "I just updated the schema for user_profiles table")

# Broadcast to all peers in the circle:
broadcast("Breaking change: BigQuery dataset 'analytics' is being renamed to 'analytics_v2'")

# Check your own identity:
whoami()
```

**The query flow**: When peer A calls `ask_peer("terraform", "question")`, the question appears in terraform's Claude session as `@data-platform asks: question`. Terraform's Claude answers, and the response is automatically captured and returned to peer A.

## 8. Managing the Team

### Check Status Anytime

```bash
repowire status          # Overall health
repowire peer list       # All peers with status
repowire top             # Interactive TUI
```

### Kill a Specific Peer

```bash
# From TUI: press 'k' on the peer
# Or from CLI - kill the tmux window:
tmux kill-window -t data-ml-platform:terraform
```

### Move a Peer to a Different Circle

From TUI: press `c` on the peer, type new circle name.

### Prune Offline Peers

```bash
repowire peer prune            # interactive confirmation
repowire peer prune --force    # skip confirmation
repowire peer prune --dry-run  # preview only
```

### View Communication Events

From TUI: press `e` to open the event log.

Or via API:

```bash
curl http://127.0.0.1:8377/events | python -m json.tool
```

### Web Dashboard

```bash
repowire build-ui   # one-time build
open http://localhost:8377/dashboard
```

## 9. Tear Down the Team

Kill all peers in the circle by killing the tmux session:

```bash
tmux kill-session -t data-ml-platform
```

Then prune the registry:

```bash
repowire peer prune --force
```

## 10. Quick Reference

| Task | Command |
|------|---------|
| Check everything | `repowire status` |
| Spawn peer | `repowire peer new PATH --circle data-ml-platform` |
| List peers | `repowire peer list` |
| Interactive dashboard | `repowire top` |
| Attach to session | `tmux attach -t data-ml-platform` |
| Kill one peer | `tmux kill-window -t data-ml-platform:PEER` |
| Kill all peers | `tmux kill-session -t data-ml-platform` |
| Clean up registry | `repowire peer prune --force` |
| View logs | `tail -f ~/.repowire/daemon.log` |
| Restart daemon | `repowire service restart` |

## 11. Tips

- **Peer names are folder names**. `data-platform`, `terraform`, etc. are auto-derived.
- **All peers in the same circle can talk to each other**. Cross-circle communication is blocked by default.
- **The daemon must be running** for peer communication. Peers still spawn without it, but can't message each other.
- **Config lives at** `~/.repowire/config.yaml`. Peers auto-register via hooks on SessionStart.
- **Pending queries** are stored in `~/.repowire/pending/`. If something gets stuck, clear this directory.
- **Mix backends freely** - put some peers on Claude Code and others on OpenCode in the same circle. The daemon routes to the correct backend per peer.
