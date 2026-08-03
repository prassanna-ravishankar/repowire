<div align="center">
  <picture>
    <source srcset="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-dark.webp" media="(prefers-color-scheme: dark)" width="150" height="150" />
    <img src="https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/images/logo-light.webp" alt="Repowire Logo" width="150" height="150" />
  </picture>

  <h1>Repowire</h1>
  <p>Let your coding agents talk to each other.</p>

  [![Release](https://img.shields.io/github/v/release/prassanna-ravishankar/repowire)](https://github.com/prassanna-ravishankar/repowire/releases)
  [![CI](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml/badge.svg)](https://github.com/prassanna-ravishankar/repowire/actions/workflows/ci.yml)
  [![Go](https://img.shields.io/github/go-mod/go-version/prassanna-ravishankar/repowire?filename=daemon-go%2Fgo.mod)](daemon-go/go.mod)
  [![License](https://img.shields.io/github/license/prassanna-ravishankar/repowire)](LICENSE)
  [![Docs](https://img.shields.io/badge/docs-repowire.io-2563EB)](https://docs.repowire.io/)
</div>

Repowire connects the coding agents you already have open. Claude Code in one repo, Codex in another, a dashboard in your browser, Telegram on your phone: Repowire gives them names and lets them pass messages without copy-paste.

It is a local control layer for multi-agent work: ask another agent a question, send a quick update, schedule a reminder, or run one session as the coordinator.

Use it when:

- One repo needs a concrete answer from an agent already working in another repo.
- You want a personal orchestrator session to dispatch tasks, collect status, or keep reviews moving.
- You want to monitor or nudge agent work from your phone or browser.
- A session should wake itself or another peer later with a scheduled check-in.

Repowire runs locally by default through a daemon on your machine. The hosted relay is optional and uses outbound connections for remote dashboard access and cross-machine mesh traffic.

## Quickstart

**Requirements:** macOS or Linux and tmux. Python is not required.

**1. Install Repowire and wire your agents.**

```bash
curl -fsSL https://github.com/prassanna-ravishankar/repowire/releases/latest/download/install.sh | sh
```

The installer downloads a checksum-verified native binary from GitHub Releases.

**2. Open your normal agent CLIs.**

Use the tools directly; Repowire hooks into them after setup.

```bash
# tmux window 1
cd ~/projects/project-a && claude

# tmux window 2
cd ~/projects/project-b && codex
```

**3. Check that both peers appeared.**

Claude Code registers on session start. Codex registers through App Server when
its TUI thread opens, before the first prompt. Run:

```bash
repowire peer list
```

**4. Ask from one agent to the other.**

In `project-a`, tell your local agent:

```text
Ask project-b what API endpoints they expose.
```

Your local agent invokes Repowire's `ask` MCP tool, the second agent receives the question, and the reply comes back as an `ack` notification. Repowire is the mesh and tool surface around the agents, not a standalone chat UI. The same pattern works across Claude Code, Codex, Gemini CLI, OpenCode, and Pi when those runtimes are installed.


https://github.com/user-attachments/assets/a9eab9c4-8aea-4dbb-8914-e998311b6d14


You can also spawn peers through Repowire:

```bash
repowire peer new ~/projects/project-a
repowire peer new ~/projects/project-b --backend codex --profile fast
```

For durable recurring workers, scaffold a repo-local agent folder and target it
from jobs:

```bash
repowire agents create daily-brief --backend codex
repowire jobs create "Daily brief" --path .repowire/agents/daily-brief --backend codex --cron "@daily" --prompt "Prepare the brief."
```

Full docs: [docs.repowire.io](https://docs.repowire.io).

## What You Get

- **Agent-to-agent asks**: Non-blocking questions with explicit `ack` replies and reminder injection until a thread is closed.
- **Human control surfaces**: Browser dashboard, Telegram, and Slack can route messages as service peers.
- **Durable jobs**: Track one-off and recurring work through CLI/MCP, with dashboard visibility and controls for run, retry, and cancel.
- **Orchestrator pattern**: A dedicated peer can dispatch work, check status, coordinate reviews, and keep a queue moving.
- **Scheduled wake-ups**: Send a future notification or ask to yourself, another peer, or an orchestrator.
- **Optional relay**: Reach the dashboard remotely and bridge machines without opening inbound ports.

## How It Works

All peers connect to a local daemon. The daemon keeps the registry, routes asks/notifies, tracks open asks, stores durable jobs, runs schedules, and feeds the dashboard timeline.

<p align="center">
  <img src="images/repowire-arch.webp" alt="Repowire architecture" width="700" />
</p>

The stable public surface is peers, circles, asks, notifications, broadcasts, schedules, and the `/jobs` tracked-work API exposed through CLI and MCP JSON tools. The v0.14 direction is session-native: sessions become the durable unit of work, while peers remain the live runtime executors. The current dashboard shows the selected peer/session view, merges Claude transcript history where available with realtime events, and is moving toward broader session commands for controls like resume, scheduling, approvals, and future backend/model changes.

Transport notes:

- Claude Code and Gemini CLI use hooks plus MCP tools.
- Codex uses its native App Server thread API plus MCP; tmux remains optional lifecycle/placement support.
- OpenCode uses a TypeScript plugin plus WebSocket.
- Pi uses the Repowire extension path when detected by setup.
- Claude Code channel/ACP delivery is experimental and opt-in.
- Relay is optional remote access, not a requirement for local routing.

## Supported Agents and Surfaces

| Agent runtime | Connection path |
| --- | --- |
| Claude Code | Hooks + MCP; optional experimental channel/ACP transport |
| Codex | App Server threads + MCP |
| Gemini CLI | Hooks + MCP through normalized `BeforeAgent` / `AfterAgent` events |
| Antigravity CLI (`agy`) | Plugin install verified; hook firing and MCP pending upstream verification |
| OpenCode | Plugin + WebSocket |
| Pi | Repowire extension |

| Human or service surface | Role in the mesh |
| --- | --- |
| Dashboard | Browser control surface at `localhost:8377/dashboard` or through relay |
| Telegram | Phone control surface and notification target |
| Slack | Team chat control surface |
| Orchestrator peer | Long-running coordinator that dispatches and reviews work |
| Relay dashboard | Optional remote dashboard and cross-machine bridge |

`repowire setup` auto-detects installed runtimes and wires the supported transports it finds.

## How It Compares

The agent-orchestration space is moving fast. Most projects cluster around a few shapes:

- **Worktree/task runners**: [Claude Squad](https://github.com/smtg-ai/claude-squad), [Vibe Kanban](https://github.com/BloopAI/vibe-kanban), and [dmux](https://dmux.ai/) help launch and review many isolated agent workspaces.
- **Deterministic schedulers**: [Bernstein](https://github.com/sipyourdrink-ltd/bernstein) decomposes goals, runs agents in parallel worktrees, verifies, and merges passing work.
- **Hierarchical swarms**: [multi-agent-shogun](https://github.com/yohey-w/multi-agent-shogun) defines manager/worker roles and routes tasks through tmux, files, or role-specific protocols.
- **Agent IDEs and workflow systems**: [HumanLayer/CodeLayer](https://github.com/humanlayer/humanlayer) focuses on planning, review, team workflows, and richer agent workspaces.

Repowire sits in a different slot: it is a live mesh and control plane for agent sessions you already have running. It does not try to be the scheduler that decomposes every goal, the kanban board that owns every branch, or the merge gate that lands code. It gives your existing terminals, dashboard, Telegram, Slack, and orchestrator session a shared address book, message lifecycle, schedule queue, and local session timeline.

## Common Workflows

### Ask another repo

Project A needs the real API shape from Project B. Ask `project-b`; the peer answers from its live checkout, not stale docs. See [multi-repo coordination](https://docs.repowire.io/use/workflows/multi-repo/).

### Drive from phone or dashboard

Send work to a peer from Telegram, Slack, or the dashboard, then receive progress updates from agents as notifications. Telegram and Slack human messages open tracked asks by default; use their notify/FYI commands for fire-and-forget nudges. See [mobile mesh management](https://docs.repowire.io/use/workflows/mobile-mesh/).

### Coordinate with an orchestrator

Run one session as the orchestrator. It can dispatch to project peers, ask for status, review PRs, and wake itself later. See [orchestrator coordination](https://docs.repowire.io/use/workflows/orchestrator-coordination/).

### Wake a peer later

Schedule a reminder, check-in, or future ask:

```bash
repowire schedule self 10m "check CI"
repowire schedule create orchestrator 1h "handoff" --from-peer project-a --kind ask
```

### Bridge machines

Enable the hosted relay when you want remote dashboard access or cross-machine mesh traffic:

```bash
repowire setup --relay
```

## Dashboard

<p align="center">
  <img src="images/repowire-hosted-2.png" alt="Repowire dashboard peer overview" width="700" />
</p>

The dashboard shows peers, status, descriptions, chat turns, tool calls, attachments, durable jobs, and the selected peer/session timeline. For Claude Code peers, it can merge transcript history with realtime events; other backends contribute realtime events as their transports report them.

Run it locally at:

```text
http://localhost:8377/dashboard
```

With relay enabled, use:

```text
https://repowire.io/dashboard
```

## Core Commands

```bash
repowire setup                         # install runtime transports/service + localhost MCP identity shim
repowire setup --update-checks         # let status/doctor report available updates
repowire update                        # explicit package upgrade + hook reinstall + daemon restart
repowire status                        # show installed components and daemon status
repowire doctor                        # run diagnostics
repowire service restart               # restart the installed daemon service
repowire peer list                     # list mesh peers
repowire peer new PATH [--profile P]   # spawn a peer in tmux
repowire schedule self 10m "check CI"  # wake this peer later
repowire telegram start                # run Telegram service peer
repowire slack start                   # run Slack service peer
```

The daemon uses `~/.repowire/state.db` for durable local state. On first startup
after install or update, it applies SQLite migrations and imports legacy
`schedules.json`, `events.json`, and `sessions.json` once while leaving those
files in place for downgrade/export compatibility. Migrated state is written to
SQLite, and `repowire doctor` reports the SQLite schema, integrity, and import
status.

See the full [CLI reference](https://docs.repowire.io/reference/cli/) and [MCP tools reference](https://docs.repowire.io/reference/mcp-tools/).

## Configuration and Security

Config lives at `~/.repowire/config.yaml`.

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "rw_local_..."
  mcp_http:
    enabled: true
    bind: "localhost-only"
    require_auth: true
    allow_dangerous_tools: false
  spawn:
    commands:
      claude-code: "claude --dangerously-skip-permissions"
      codex: "codex --dangerously-bypass-approvals-and-sandbox"
      gemini: "gemini --yolo"
    profiles:
      codex:
        fast:
          args: ["--model", "gpt-5-mini"]
    allowed_paths: [~/git, ~/projects]
updates:
  check_enabled: false

relay:
  enabled: true
  url: "wss://repowire.io"
  api_key: "rw_..."
```

Update checks are off by default. If enabled with `repowire setup --update-checks`, `repowire status` and `repowire doctor` may report that a newer release is available, but they do not rewrite hooks or restart services. Use `repowire update` when you want to upgrade explicitly.

Security defaults:

- Local daemon binds to `127.0.0.1`.
- Relay is opt-in and uses outbound WebSocket.
- WebSocket and local HTTP auth are available through `daemon.auth_token`.
- MCP tools are implemented by the Go daemon at a localhost-only, bearer-authenticated `/mcp`; agent runtimes reach it through a thin stdio identity shim, and the hosted relay rejects it.
- Spawn requires explicit command and path allowlists.
- Experimental channel/ACP transport is opt-in.

## Developing From Source

```bash
git clone https://github.com/prassanna-ravishankar/repowire
cd repowire
cd web && npm ci && npm run build && cd ..
mkdir -p bin
(cd daemon-go && go build -o ../bin/repowire .)
./bin/repowire setup --non-interactive
```

Hooks and the MCP identity shim run the binary recorded by setup, not an
arbitrary binary elsewhere in your checkout. Rebuild that binary after native
changes, then restart the daemon service:

```bash
(cd daemon-go && go build -o ../bin/repowire .)
./bin/repowire setup --non-interactive   # rewrites hooks/MCP/service to this build
./bin/repowire service restart           # enough when only daemon code changed
```

If service management fails, use `repowire service status` first. Raw `launchctl` on macOS or `systemctl --user` on Linux are fallback troubleshooting tools.

## References

- [Start](https://docs.repowire.io/start/)
- [Architecture](https://docs.repowire.io/operate/architecture/)
- [MCP tools](https://docs.repowire.io/reference/mcp-tools/)
- [CLI](https://docs.repowire.io/reference/cli/)
- [Use](https://docs.repowire.io/use/)
- [Features](https://docs.repowire.io/use/features/)
- [Troubleshooting](https://docs.repowire.io/troubleshooting/)
- [Compare](https://docs.repowire.io/start/compare/)

## Uninstall

```bash
repowire uninstall
rm -f ~/.local/bin/repowire
rm -rf ~/.local/share/repowire
```

`repowire uninstall` removes hooks, MCP entries, channel transport config, OpenCode plugin files, and the daemon service. It does not automatically remove `~/.repowire/`, which contains local config, events, attachments, and relay keys.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Before opening a PR, run the advisory repo-hygiene
checklist:

```bash
scripts/pre-pr-hygiene.sh
```

It is an opt-in prompt for docs, README, agent-instruction, and graphify follow-ups, not a
mandatory hook. It also flags Beads JSONL ledger churn before it can leak into PR diffs.

## License

MIT
