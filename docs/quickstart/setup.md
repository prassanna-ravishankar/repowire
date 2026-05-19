# Setup

```bash
repowire setup
```

This is the one-time install step. It runs from your shell, not from inside an agent.

## What setup does

For every agent runtime it finds, it wires the appropriate Repowire transport. Auto-detection covers Claude Code, Codex, Gemini CLI, OpenCode, and Pi. Then it installs the local daemon as a user service (launchd on macOS, systemd on Linux).

When setup finishes, the daemon is listening on `127.0.0.1:8377`. Open a new agent session in any directory and it will register itself.

`repowire setup` owns Repowire's transport and routing layer. It does not install third-party skills or Claude Code marketplace plugins; those can be installed separately if you want reusable agent behaviors on top of the mesh.

## Useful flags

```bash
repowire setup --relay                  # also connect to the hosted relay at repowire.io
repowire setup --experimental-channels  # use the MCP channel transport (Claude Code v2.1.80+, claude.ai login, bun)
repowire setup --non-interactive        # take flag values only, no prompts
```

`--relay` makes the dashboard available at `https://repowire.io/dashboard` over an outbound WebSocket. See [hosted relay](../relay/hosted.md).

`--experimental-channels` replaces tmux-injection delivery with direct MCP-channel / ACP delivery for Claude Code only. It is experimental. See [Claude Code setup](../agents/claude-code.md).

## Verifying

```bash
repowire status
```

Shows what was installed, which agents were detected, and whether the daemon is running. If something looks off, see [troubleshooting](../troubleshooting/index.md).

You only run `repowire setup` once per machine. Next: [open two agents and route an ask](first-ask.md).
