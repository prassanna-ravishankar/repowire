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
| `Stop` | Extracts response + tool calls from the transcript, delivers any pending legacy `/query`, fetches `/asks/pending` and emits a reminder block if open asks exist, then marks the peer `online` |
| `StopFailure` | Uses the same stop handler to repair status after API-level stop failures |
| `SessionEnd` | Tears down the WebSocket hook supervisor, marks the peer `offline` |

The hooks shell out to the `repowire` CLI: `repowire hook session`, `repowire hook prompt`, `repowire hook notification`, `repowire hook stop`, `repowire hook session-end`.

### MCP server

The normal Repowire MCP server is added as `repowire`. It runs as `repowire mcp` over stdio and provides the stable tool surface: `ask`, `ack`, `notify_peer`, `broadcast`, peer listing, schedules, and related commands.

## Plugins and skills

Claude Code has its own plugin and skill distribution surfaces. Repowire does not currently ship as a Claude Code marketplace plugin, and `repowire setup` does not install third-party skills.

You can still use those surfaces alongside Repowire:

- [Vercel Labs `skills`](https://github.com/vercel-labs/skills) installs reusable `SKILL.md` packages across agents, for example `npx skills add vercel-labs/agent-skills -a claude-code`.
- [Claude Code plugin marketplaces](https://code.claude.com/docs/en/discover-plugins) install plugins that can bundle skills, agents, hooks, MCP servers, LSP servers, and settings. Claude Code exposes them through `/plugin` and `claude plugin ...` commands.

Treat these as capability packaging layers. Repowire's Claude Code integration remains the hooks + MCP transport described above.

## Channel transport (experimental)

```bash
repowire setup --experimental-channels
```

Replaces tmux-injection delivery with direct MCP-channel delivery. When a message arrives, Claude sees a `<channel source="repowire">` tag in its context instead of a `[ask #cid from @peer] ...` line injected into the terminal.

Channel setup adds a separate `repowire-channel` MCP server entry in `~/.claude.json`. The normal `repowire` MCP server remains installed; use its stable tools, including `ack`, for ask lifecycle and parity with the default transport. The channel server itself only handles channel delivery and legacy query replies.

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

- Hooks not firing → [Hooks not firing](../troubleshooting/hooks.md).
- Channel auth errors → [Channel-mode auth failures](../troubleshooting/channel-auth.md).
- Peer stuck `busy` after a turn ends → [Ghost peers and stuck busy state](../troubleshooting/ghost-peers.md).
