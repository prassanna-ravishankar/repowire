# Claude Code

`repowire setup` configures Claude Code automatically. This page explains what was wired and what to check when it didn't.

## What gets installed

Hooks land in `~/.claude/settings.json`. The normal Repowire MCP server is added by the Claude CLI. In experimental channel mode, the channel server entry is written to `~/.claude.json`.

### Hooks

Six lifecycle events are wired by default:

| Event | What it does |
| --- | --- |
| `SessionStart` | Registers the peer with the daemon, spawns the WebSocket hook supervisor, injects the peer list as context |
| `UserPromptSubmit` | Marks the peer `busy` |
| `Notification` | Resets the peer to `online` when Claude Code emits an idle prompt |
| `Stop` | Extracts response + tool calls from the transcript, drains any old legacy `/query` FIFO response, fetches `/asks/pending` and emits a reminder block if open asks exist, then marks the peer `online` |
| `StopFailure` | Uses the same stop handler to repair status after API-level stop failures |
| `SessionEnd` | Tears down the WebSocket hook supervisor, marks the peer `offline` |

The hooks shell out to the `repowire` CLI: `repowire hook session`, `repowire hook prompt`, `repowire hook notification`, `repowire hook stop`, `repowire hook session-end`.

### Native inbox delivery

Claude Code 2.1.224 and newer exposes each session's local messaging socket to
hooks. Repowire's WebSocket hook prefers that native inbox for inbound asks and
notifications, so an idle session starts a turn and a busy session receives the
message between tool calls without terminal keystrokes. The hook authenticates
with Claude's per-session messaging token, so Claude recognizes it as its own
installed hook. A successful socket write is recorded as native runtime
acceptance, not pane injection.

Tmux injection remains the automatic fallback when Claude does not expose the
socket or a socket write fails. Claude's `crossSessionInbound` policy still
applies and may hold or refuse an accepted message; Repowire does not change
that setting. Current setup therefore still requires tmux for placement and
lifecycle ownership while the session-keyed bridge is tested separately.

### MCP server

The normal Repowire MCP entry is added as `repowire`. `repowire mcp` runs as a
small stdio identity shim and proxies the stable tool surface (`ask`, `ack`,
`notify_peer`, schedules, jobs, and related commands) to the Go daemon's local
`/mcp` endpoint.

## Plugins and skills

Claude Code has its own plugin and skill distribution surfaces. Repowire does not currently ship as a Claude Code marketplace plugin, and `repowire setup` does not install third-party skills.

You can still use those surfaces alongside Repowire:

- [Vercel Labs `skills`](https://github.com/vercel-labs/skills) installs reusable `SKILL.md` packages across agents, for example `npx skills add vercel-labs/agent-skills -a claude-code`.
- [Claude Code plugin marketplaces](https://code.claude.com/docs/en/discover-plugins) install plugins that can bundle skills, agents, hooks, MCP servers, LSP servers, and settings. Claude Code exposes them through `/plugin` and `claude plugin ...` commands.

Treat these as capability packaging layers. Repowire's Claude Code integration remains the hooks + MCP transport described above.

A future optional Repowire marketplace plugin should package Claude Code-facing commands, skills, docs, and an MCP bootstrap around the existing `repowire mcp` command. It must not replace `repowire setup`, install a second daemon, own `~/.repowire/config.yaml`, or redefine ask/ack/notify behavior. See [Claude Code plugin packaging](../../contributing/design-notes/claude-code-plugin-packaging.md) for the proposed layout, version/manifest drift checks, and install/update/uninstall docs impact.

## Channel transport (experimental)

```bash
repowire setup --experimental-channels
```

The session-owned [Claude Channel bridge](../../concepts/bridges.md) replaces
tmux-injection delivery with direct MCP-channel delivery. When a message
arrives, Claude sees a `<channel source="repowire">` tag in its context instead
of a `[ask #cid from @peer] ...` line injected into the terminal.

Channel setup adds a separate `repowire-channel` MCP server entry in `~/.claude.json`. The normal `repowire` MCP server remains installed; use its stable tools, including `ack`, for ask lifecycle and parity with the default transport. The channel server itself only handles channel delivery.

Requirements:

- Claude Code v2.1.80 or newer.
- claude.ai login (not API/Console key auth).
- [bun](https://bun.sh) on `PATH`.

The channel transport only replaces the SessionStart / UserPromptSubmit / Notification hooks. The `Stop` and `StopFailure` hooks are kept so the dashboard still sees chat turns and status repair still runs. If the daemon has `daemon.auth_token` configured, setup passes it to the channel server as `REPOWIRE_AUTH_TOKEN` so the WebSocket registration can authenticate.

If `repowire setup --experimental-channels` declines to install:

- *"bun runtime not found"* — install bun, then re-run.
- *"Claude Code vX.Y.Z doesn't support channels"* — upgrade Claude Code to v2.1.80+ and re-run.

## Verifying

```bash
repowire status
```

Shows whether Claude Code is detected, whether hooks are installed, and whether the channel transport is on.

To confirm hooks fire, open a new Claude Code session in tmux and watch `repowire peer list`. The peer should appear within a few seconds of the first prompt.

## Troubleshooting

- Hooks not firing → [Hooks not firing](../../troubleshooting/hooks.md).
- Native messages fall back to terminal injection → confirm Claude Code is
  2.1.224 or newer and `/status` shows a `Peer address`.
- Channel auth errors → [Channel-mode auth failures](../../troubleshooting/channel-auth.md).
- Peer stuck `busy` after a turn ends → [Ghost peers and stuck busy state](../../troubleshooting/ghost-peers.md).
