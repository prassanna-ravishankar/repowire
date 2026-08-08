# Setup

```bash
repowire setup
```

This is the one-time install step. It runs from your shell, not from inside an agent.

## What setup does

For every agent runtime it finds, the Go CLI wires the appropriate Repowire
transport. Auto-detection covers Claude Code, Codex, Gemini CLI, Antigravity,
OpenCode, and Pi. Then it installs the Go daemon as a user service (launchd on
macOS, systemd on Linux). When Codex exposes an App Server Unix listener, setup
also installs its independent thread bridge and keeps only Codex's
pending-ask reminder Stop hook.

When setup finishes, the daemon is listening on `127.0.0.1:8377`. Open a new agent session in any directory and it will register itself.

`repowire setup` owns Repowire's transport and routing layer. It does not install third-party skills or Claude Code marketplace plugins; those can be installed separately if you want reusable agent behaviors on top of the mesh.

## Useful flags

```bash
repowire setup --relay                  # also connect to the hosted relay at repowire.io
repowire setup --experimental-channels  # use the experimental channel/ACP transport (Claude Code v2.1.80+, claude.ai login, bun)
repowire setup --http-mcp               # accepted for compatibility; /mcp is configured by setup
repowire setup --update-checks          # let status/doctor report new Repowire releases
repowire setup --non-interactive        # take flag values only, no prompts
```

`--relay` makes the dashboard available at `https://repowire.io/dashboard` over an outbound WebSocket. See [relay access](../use/features/relay-access.md).

`--experimental-channels` replaces the default hooks transport's tmux-injection delivery with direct MCP-channel / ACP delivery for Claude Code only. It is experimental. See [Claude Code setup](../use/features/connect-claude-code.md).

Setup always enables the localhost-only `/mcp` implementation and generates a
`daemon.auth_token` if needed, because agent runtimes reach it through
`repowire mcp`. That stdio command is a small Go proxy which preserves the
runtime's daemon-minted peer identity; it does not duplicate the MCP tools.
`--http-mcp` remains accepted for older scripts. Direct HTTP clients must send
`Authorization: Bearer <token>` and, without the stdio identity proof, act as
the restricted `mcp-http` caller.

`--update-checks` opts this machine into update availability checks from `repowire status` and `repowire doctor`. It only reports that a newer release exists; `repowire update` remains the explicit command that upgrades the installed binary and re-runs setup.

## Verifying

```bash
repowire status
```

Shows what was installed, which agents were detected, and whether the daemon is running. If something looks off, see [troubleshooting](../troubleshooting/index.md).

You only run `repowire setup` once per machine. Next: [open two agents and route an ask](first-ask.md).
