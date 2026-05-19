<div align="center">
  <picture>
    <source srcset="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-dark.webp" media="(prefers-color-scheme: dark)" width="150" height="150" />
    <img src="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-light.webp" alt="Repowire Logo" width="150" height="150" />
  </picture>

  <h1>Repowire</h1>
  <p>Mesh network for AI coding agents. Enables Claude Code, Codex, Gemini, and OpenCode sessions to communicate.</p>

  [![PyPI](https://img.shields.io/pypi/v/repowire)](https://pypi.org/project/repowire/)
  [![CI](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml/badge.svg)](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml)
  [![Python](https://img.shields.io/pypi/pyversions/repowire)](https://pypi.org/project/repowire/)
  [![License](https://img.shields.io/pypi/l/repowire)](https://github.com/prassanna-ravishankar/repowire/blob/main/LICENSE)
  [![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/prassanna-ravishankar/repowire)
</div>

## Why?

AI coding agents work great in isolation, but real projects need agents that **talk to each other**. An agent in one repo needs answers from another repo. You need to dispatch work to multiple agents from your phone. A dedicated orchestrator needs to coordinate 10+ peers across projects.

Repowire connects your agents into a live mesh. Any agent can query, notify, or broadcast to any other. You manage the mesh from a dashboard, Telegram, or Slack. It's local-first, works across agent runtimes, and scales from 2 peers to 20+.

Read more: [the context breakout problem](https://prassanna.io/blog/vibe-bottleneck/) and [the idea behind Repowire](https://prassanna.io/blog/repowire/).

**Current release:** v0.13.32 bundles the transport-router extraction for ask/notify delivery, the graphify architecture report, refreshed README/docs/images, and the session-scoped dashboard timeline that merges the selected peer/session Claude transcript with realtime events.

<details>
<summary><strong>How does repowire compare?</strong></summary>

| Project | Type | How it works | Best for |
|---------|------|--------------|----------|
| **Repowire** | Sync | Live agent-to-agent queries | Cross-repo collaboration, 5-10 peers |
| **[Gastown](https://github.com/steveyegge/gastown)** | Async | Work orchestration with persistent mail | Coordinated fleets, 20-30 agents |
| **[Claude Squad](https://github.com/smtg-ai/claude-squad)** | Isolated | Session management with worktrees | Multiple independent sessions |
| **[Memory Bank](https://docs.tinyfat.com/guides/memory-bank/)** | Async | Structured markdown files | Persistent project knowledge |

Repowire is a phone call (real-time, ephemeral). Gastown is email + project manager (async, persistent). For 5-10 agents, emergence works. For 20-30 grinding through backlogs, you probably need structure.

</details>

https://github.com/user-attachments/assets/3755328a-e38a-45fb-b465-34526afbe44a

## Installation

**Requirements:** macOS or Linux, Python 3.10+, tmux

```bash
# One-liner (detects uv/pipx/pip, runs interactive setup)
curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh

# Or install manually
uv tool install repowire    # or: pipx install repowire / pip install repowire
```

Developing from source:

```bash
git clone https://github.com/prassanna-ravishankar/repowire
cd repowire
uv sync --extra dev
uv tool install . --force-reinstall
```

Hooks and MCP servers run the installed `repowire` executable, not your checkout. After changing daemon, hook, or MCP code locally, reinstall the tool and restart the daemon service so the live mesh uses the new code:

```bash
uv tool install . --force-reinstall
repowire setup --non-interactive   # rewrites hooks/MCP/service to the installed local build

# If only daemon code changed, restarting the service is enough:
launchctl unload ~/Library/LaunchAgents/io.repowire.daemon.plist && \
  launchctl load ~/Library/LaunchAgents/io.repowire.daemon.plist
# Linux: systemctl --user restart repowire
```

## Quick Start

```bash
# One-time setup: detects your agents, installs hooks + MCP, starts daemon
repowire setup
```

Then open your agents in separate tmux windows:

```bash
# tmux window 1
cd ~/projects/project-a && claude

# tmux window 2
cd ~/projects/project-b && codex
```

Both sessions auto-register as peers and discover each other. In project-a:

```
"Ask project-b what API endpoints they expose"
```

The agent calls `ask`, project-b receives the question and acks back with `ack(corr_id, "...")`. The reply lands in project-a as a notification framed `[ack #cid from @project-b] ...`. Works across Claude Code, Codex, Gemini CLI, and OpenCode in any mix.

Or use the CLI helper to spawn sessions in tmux:

```bash
repowire peer new ~/projects/project-a
repowire peer new ~/projects/project-b
```

## How It Works

All peers connect to a central daemon via **WebSocket**. The daemon routes addressed messages between peers. No pub/sub, no topics. Messages go from peer A to peer B by name.

<p align="center">
  <img src="images/repowire-arch.webp" alt="Repowire architecture" width="700" />
</p>

**Message types:**
- `ask` - non-blocking. Returns a correlation_id immediately; the recipient closes the thread with `ack(corr_id)` (bare close, "seen, no action") or `ack(corr_id, message)` (close with reply, delivered to the asker as a notification framed `[ack #cid from @peer] message`). Chain follow-ups with `ask(reply_to=corr_id, ...)`.
- `ack` - close an open ask thread. Bare or with a reply message.
- `notify_peer` - fire-and-forget (no lifecycle, no response expected)
- `broadcast` - fan-out to all peers in your circle

**Circles** are logical subnets (mapped to tmux sessions). Peers can only communicate within their circle unless explicitly bypassed.

### Current Direction: Session-Native Mesh

Repowire's v0.13 architecture train is incrementally shifting the product boundary from a transient peer connection to a durable session. The ask/notify transport-router extraction has landed; the remaining sequence adds a session/timeline store, updates the dashboard to render persisted and realtime conversation state together, moves composer and control actions onto session commands, and centralizes runtime lifecycle plus approval events. The goal is compatibility first: existing peer, hook, MCP, and dashboard workflows should keep working while the internals become session-native.

This is roadmap/current direction, not a claim that every item is fully shipped today:

- **Session-first mesh.** Repowire is moving toward sessions as the durable unit of work, with peers acting as live runtime executors.
- **Transport-neutral routing.** Ask/notify delivery now goes through a transport router. WebSocket hooks, experimental ACP, relay, and future transports continue moving toward the same message/control boundary.
- **Timeline-centered dashboard.** The roadmap is to merge persisted history and realtime events into one session timeline instead of separate live/history views.
- **Shared command surface.** Controls such as send message, switch backend/model, resume, schedule, and approvals should target sessions and be reusable from dashboard, MCP, Telegram, and other surfaces.
- **Incremental v0.13 train.** Compatible v0.13.x slices preserve current hooks/MCP/HTTP behavior while the session-native model hardens.
- **Human approval path.** Permission and plan approval events are expected to become first-class timeline/control events instead of transport-specific callbacks.

The current public surface still exposes peers, circles, asks, notifications, and schedules. Do not assume model switching, plan approval, reliable delivery across every transport, production-ready ACP, or fully transport-neutral routes/control paths are complete today.

### Supported Agents

| Agent | Transport | How it connects |
|-------|-----------|----------------|
| **Claude Code** | Hooks + MCP | Lifecycle hooks register peer, MCP tools for messaging |
| **OpenAI Codex** | Hooks + MCP | Same pattern (requires `codex_hooks` feature flag, auto-enabled) |
| **Google Gemini CLI** | Hooks + MCP | Uses `BeforeAgent`/`AfterAgent` events (mapped to prompt/stop hooks) |
| **OpenCode** | Plugin + WebSocket | TypeScript plugin with persistent WS connection |

`repowire setup` auto-detects which agents are installed and configures each one.

All agents use **hooks + tmux injection** for message delivery:
- **SessionStart** - registers peer, spawns WebSocket hook, injects peer list
- **UserPromptSubmit** / **BeforeAgent** - marks peer BUSY
- **Stop** / **AfterAgent** - marks peer ONLINE, extracts response for dashboard

### Skills and Plugin Marketplaces

Repowire installs its own hooks, MCP server, daemon service, and OpenCode plugin. It does not currently install third-party agent skills or publish a Claude Code plugin marketplace entry.

Those ecosystems are complementary:

- [Vercel Labs `skills`](https://github.com/vercel-labs/skills) installs reusable `SKILL.md` packages across agents with commands like `npx skills add vercel-labs/agent-skills -a claude-code` or `-a codex`.
- [Claude Code plugin marketplaces](https://code.claude.com/docs/en/discover-plugins) distribute Claude Code plugins that can bundle skills, agents, hooks, MCP servers, and related config. Manage them from Claude Code's `/plugin` UI or `claude plugin ...` commands.

Use those when you want reusable agent behavior on top of the Repowire mesh. Use `repowire setup` for Repowire's transport and routing layer.

<details>
<summary><strong>Experimental: Claude Code channel transport</strong></summary>

On Claude Code v2.1.80+ with claude.ai login and [bun](https://bun.sh), an experimental **channel transport** delivers messages directly via MCP, with no tmux injection.

```bash
repowire setup --experimental-channels
```

- Messages arrive as `<channel source="repowire">` tags in Claude's context
- Claude replies via `reply` tool instead of transcript scraping
- Requires claude.ai login (not available for API/Console key auth)

</details>

## Patterns

<details>
<summary>Multi-repo coordination</summary>

Agents in different repos ask each other questions in real time. Project-a needs to know project-b's API shape? `ask("project-b", "what endpoints do you expose?")` opens a thread; project-b replies with `ack(corr_id, "POST /users, GET /users/:id, ...")` and project-a sees a live answer from the actual codebase, not stale docs.
</details>

<details>
<summary>Cross-agent review</summary>

Have a different agent review your work. Peer A builds a feature, peer B runs a review pass (code quality, security, simplification). Works especially well with different agent runtimes reviewing each other's output.
</details>

<details>
<summary>Orchestrator</summary>

A dedicated coordinator peer manages the mesh. It dispatches tasks, tracks progress, runs review cycles, and coordinates releases across multiple project peers. The pattern that makes 10+ agents manageable.

The orchestrator loop:

1. Scan a GitHub project board for `Todo` items.
2. Claim by `notify_peer`-ing the target project peer with the task brief; flip the board item to `In Progress`.
3. Receive `ask`/`notify` updates as work progresses (`set_description` keeps the dashboard honest).
4. On peer reports `done`, review the PR (`review_queue` surfaces open PRs the peer touched), merge after sign-off, flip the board item to `Done`.
5. Roll a release tag when a batch lands.

A second orchestrator peer can co-exist as an observer/learner without colliding — use `orchestrator_status` to see open asks and avoid double-claiming. Pair a `claude-code` orchestrator with a `codex` or `gemini` one to keep the mesh moving when one runtime hits credit limits.
</details>

<details>
<summary>Worktree isolation</summary>

Spawn peers on git worktrees for parallel, isolated work. Each peer works on a branch, creates a PR, another peer reviews. Clean separation with no merge conflicts during development.
</details>

<details>
<summary>Mobile mesh management</summary>

The Telegram bot lets you dispatch work, check peer status, and coordinate from your phone. Send a message to any peer from anywhere.
</details>

<details>
<summary>Infrastructure-as-peer</summary>

A dedicated peer for infrastructure (k8s, DNS, cloud config) that other project peers coordinate with directly. Need a namespace created? Ask the infra peer. Need a deploy? Notify it.
</details>

<details>
<summary>Overnight autonomy</summary>

Give peers tasks and disconnect. They work autonomously, report back via Telegram or dashboard when you return. Long-running tasks (migrations, refactors, test suites) complete while you sleep.
</details>

## Control Plane

### Web Dashboard

<p align="center">
  <img src="images/repowire-hosted-2.png" alt="Peer grid overview" width="700" />
</p>

Monitor your agent mesh at `http://localhost:8377/dashboard`, or remotely via [repowire.io](https://repowire.io):

- **Peer overview** - online/busy/offline status, descriptions, project paths
- **Chat view** - selected-session timeline with Claude transcript history merged with realtime chat events and tool call details
- **Compose bar** - send notifications or queries to any peer from the browser
- **Mobile responsive** - hamburger menu, touch-friendly compose

Roadmap: the dashboard is moving toward fuller durable session controls. The current v0.13 slice merges Claude transcript history with realtime stream events for the selected peer/session; controls that change runtime behavior should attach to session commands as they land, while peer IDs remain implementation details of the runtime executor currently doing the work.

For remote access: `repowire setup --relay` connects your daemon to [repowire.io](https://repowire.io) via outbound WebSocket. Access your dashboard from any browser. No port forwarding, no VPN.

<details>
<summary>More screenshots</summary>
<br>
<p align="center">
  <img src="images/repowire-hosted-3.png" alt="Activity and message detail" width="700" />
</p>
<p align="center">
  <img src="images/repowire-hosted-1.png" alt="Chat view with relay" width="700" />
</p>
</details>

### Telegram Bot

Control your mesh from your phone. A Telegram bot registers as a peer. Notifications from agents appear in your chat, messages you send get routed to peers.

```bash
# Tokens configured via `repowire setup`, or via env vars:
repowire telegram start
```

- `/peers` - shows online peers with inline buttons
- Tap a peer → type your message → sent as notification
- Sticky routing: `/select repowire` → all messages go there until `/clear`
- Agents know `@telegram` is you. They can `notify_peer('telegram', ...)` to reach your phone

## MCP Tools

| Tool | Type | Description |
|------|------|-------------|
| `list_peers` | Query | List peers with status, circle, path, description, `last_seen`, `turn_state`. Defaults to online + caller's circle + hides self. Pass `circle='*'` for mesh-wide, `show_offline=True` + `include_self=True` to widen. Orchestrator-role callers default to mesh-wide |
| `ask` | Non-blocking | Open a thread. Returns a correlation_id immediately. Defaults to caller's circle; falls back to global lookup only when the target's role bypasses circles. Optional `reply_to=cid` chains a follow-up that closes the prior thread |
| `ack` | Close | Close an open ask thread. Bare `ack(cid)` is "seen, no action"; `ack(cid, message)` delivers a reply to the asker |
| `notify_peer` | Fire-and-forget | Send a notification (no lifecycle, no reply tracking) |
| `broadcast` | Fire-and-forget | Message all online peers in your circle |
| `whoami` | Query | Your own peer identity |
| `set_description` | Mutation | Update your task description, visible to all peers and the dashboard |
| `spawn_peer` | Mutation | Spawn a new agent session (requires [allowlist config](#configuration)) |
| `kill_peer` | Mutation | Kill a previously spawned session |
| `orchestrator_status` | Query | Check whether a live orchestrator is present in a circle (`present`, `peer_name`, `last_seen`) |
| `mark_reviewed` | Mutation | Mark a PR as reviewed at a given SHA. New commits on that PR re-surface it in your review queue |
| `review_queue` | Query | List PRs awaiting your review (or another peer's). Filter by `peer_name=...` |
| `schedule_create` | Mutation | Schedule a notification or ask to a peer at a future time (one-shot) |
| `schedule_self` | Mutation | Schedule a one-shot or recurring reminder to yourself (`fire_at` or `cron`) |
| `schedule_cron` | Mutation | Schedule a recurring cron notification or ask to another peer |
| `schedule_list` | Query | List your active schedules. `mine_only=False` shows all; `include_cron=True` appends recurrence |
| `schedule_delete` | Mutation | Remove a schedule |

`list_peers` and `whoami` return TSV (more token-efficient than JSON).

**Reminder injection.** If an agent receives an ask but doesn't ack/reply, repowire injects a reminder block at the start of every subsequent prompt until the ask is acked. Tool-call detection is the source of truth — prose `[ack #cid]` mentions don't close anything, only a real `ack()` call does. If a peer appears paused at the prompt (idle but with unhandled work), the daemon detects this and surfaces it on the dashboard.

**Misroute refusal.** Ambiguous peer names (multiple peers sharing a display name across circles) cause `ask`/`notify_peer` to refuse with a hint, instead of routing to the wrong peer. Always pass explicit `circle=...` to disambiguate. When several same-path peers have similar display names (`agentbox-codex`, `agentbox-2-codex`, ...), address the peer by the `peer_id` column from `list_peers`; ask/ack state is pinned to peer IDs once opened.

**Circle scoping.** As of v0.13.4, the MCP surface defaults to the caller's own circle. Peers with role `service`, `orchestrator`, or `human` (e.g. the Telegram bot, the orchestrator, you) bypass circles and are always visible. Pass `circle='*'` to widen any call to mesh-wide. Orchestrator peers default to mesh-wide automatically — they need the full view.

## CLI Reference

```bash
repowire setup                    # Install hooks, MCP server, daemon service
repowire setup --relay            # Same + enable remote dashboard via repowire.io
repowire setup --experimental-channels  # Use channel transport (needs claude.ai login + bun)
repowire status                   # Show what's installed and running
repowire doctor                   # Diagnostic checks (daemon, hooks, runtimes, relay, ...)
repowire serve                    # Run daemon in foreground
repowire serve --relay            # Run daemon with relay connection

repowire peer new PATH            # Spawn new peer in tmux
repowire peer new . --circle dev  # Spawn with custom circle
repowire peer list                # List peers and their status (god-view, includes offline)
repowire peer describe NAME       # Show full state for one peer (open asks, last seen, recent activity)
repowire peer describe NAME --circle 5    # Disambiguate when name exists in multiple circles
repowire peer prune               # Remove offline peers

repowire schedule self 10m "check CI"       # Wake this peer later
repowire schedule self "0 9 * * 1-5" "standup prep" --cron
repowire schedule create PEER 2026-05-19T18:00:00Z "handoff" --from-peer ME
repowire schedule create PEER "@hourly" "status?" --from-peer ME --cron --kind ask
repowire schedule list             # Show pending one-shot and recurring schedules
repowire schedule delete sched-12345678

repowire telegram start           # Run Telegram bot (config or env vars)
repowire slack start              # Run Slack bot (config or env vars)
repowire update                   # Upgrade package, reinstall hooks, restart daemon
repowire uninstall                # Remove all components (--yes to skip prompts)
```

`repowire peer list` is god-view: it returns every peer regardless of circle and includes the calling shell. The MCP `list_peers` tool defaults to a peer-facing view (online only, caller's circle, caller hidden) — see [Circle scoping](#mcp-tools).

## Configuration

Config file: `~/.repowire/config.yaml`

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "optional-secret"     # Require auth for WebSocket connections

  # Allow agents to spawn new sessions via MCP (both lists must be non-empty)
  spawn:
    allowed_commands:
      - claude
      - codex
      - gemini
    allowed_paths:
      - ~/git
      - ~/projects

relay:
  enabled: true                     # Connect to hosted relay
  url: "wss://repowire.io"
  api_key: "rw_..."                 # Auto-generated on first `repowire serve --relay`

telegram:                           # Optional, configured via `repowire setup`
  bot_token: "..."
  chat_id: "..."

slack:                              # Optional, configured via `repowire setup`
  bot_token: "xoxb-..."
  app_token: "xapp-..."
  channel_id: "C..."
```

Peers auto-register via WebSocket on session start. No manual config needed.

<details>
<summary><strong>Remote relay details</strong></summary>

```bash
repowire setup --relay
# ✓ Relay enabled
#   Dashboard: https://repowire.io/dashboard
```

Your daemon opens an outbound WebSocket to `repowire.io`. The relay bridges messages between daemons on different machines and proxies HTTP requests (dashboard, API) back through a cookie-authenticated tunnel.

```
Browser → repowire.io → enter key → cookie set → relay tunnels to local daemon
Daemon A ←WSS→ repowire.io ←WSS→ Daemon B (cross-machine mesh)
```

Self-host the relay: `repowire relay start --port 8000`

</details>

<details>
<summary><strong>Security</strong></summary>

- **WebSocket auth** - set `daemon.auth_token` in config to require bearer token for connections
- **CORS** - restricted to localhost origins (plus `repowire.io` when relay is enabled)
- **Spawn allowlist** - `daemon.spawn.allowed_commands` and `allowed_paths` must both be non-empty for MCP spawn to work
- **Channel gating** - channel transport is opt-in (`--experimental-channels`), requires claude.ai login

</details>

## Uninstall

```bash
# Remove hooks, MCP server, channel transport, and daemon service
repowire uninstall

# Remove the package itself
uv tool uninstall repowire
# or: pip uninstall repowire
```

`repowire uninstall` removes:
- Claude Code hooks + MCP server + channel transport
- Codex hooks + MCP config from `~/.codex/`
- Gemini hooks + MCP config from `~/.gemini/settings.json`
- OpenCode plugin
- Daemon launchd/systemd service

**Not removed automatically** (contains your data/config):
- `~/.repowire/` - config, session mappings, events, attachments
- Relay API key in `~/.repowire/config.yaml`

To fully clean up: `rm -rf ~/.repowire`

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
