# Claude Code

`repowire setup` configures Claude Code automatically. This page explains what was wired and what to check when it didn't.

## What gets installed

Hooks and MCP entries land in `~/.claude/settings.json`. Repowire owns these keys; user-defined hooks in the same file are preserved.

### Hooks

Five lifecycle events are wired by default:

| Event | What it does |
| --- | --- |
| `SessionStart` | Registers the peer with the daemon, spawns the WebSocket hook supervisor, injects the peer list as context |
| `UserPromptSubmit` | Marks the peer `busy` |
| `Notification` | Resets the peer to `online` when Claude Code emits an idle prompt |
| `Stop` | Extracts response + tool calls from the transcript, delivers any pending legacy `/query`, fetches `/asks/pending` and emits a reminder block if open asks exist, then marks the peer `online` |
| `SessionEnd` | Tears down the WebSocket hook supervisor, marks the peer `offline` |

The hooks shell out to the `repowire` CLI: `repowire hook session`, `repowire hook prompt`, `repowire hook notification`, `repowire hook stop`, `repowire hook session-end`.

### MCP server

The repowire MCP server is added under `mcpServers.repowire` in the same settings file. It runs as `repowire mcp` over stdio.

## Channel transport (experimental)

```bash
repowire setup --experimental-channels
```

Replaces tmux-injection delivery with direct MCP-channel delivery. When a message arrives, Claude sees a `<channel source="repowire">` tag in its context instead of a `[ask #cid from @peer] ...` line injected into the terminal.

Requirements:

- Claude Code v2.1.80 or newer.
- claude.ai login (not API/Console key auth).
- [bun](https://bun.sh) on `PATH`.

The channel transport only replaces the SessionStart / UserPromptSubmit / Notification hooks. The `Stop` hook is kept so the dashboard still sees chat turns and tool calls.

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
