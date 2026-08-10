# Spawning

## What it is

Spawning starts or restarts agent peers from configured backend commands and allowed project paths. It is the daemon-controlled alternative to opening another terminal manually.

## When to use it

Use spawning when an orchestrator, dashboard, CLI command, or MCP tool should launch another peer in a known project folder. Use manual launch when you need ad hoc terminal control outside the configured allowed paths.

Use restart when the backend supports resume and the existing peer has a captured runtime session id. Restart is strict: Repowire pre-validates resume data before killing a live pane.

## Setup

Configure spawn commands and allowed paths in `~/.repowire/config.yaml`:

```yaml
daemon:
  spawn:
    allowed_paths:
      - ~/projects
    commands:
      claude-code: claude
      codex: codex
    env_path:
      - ~/.local/bin
      - ~/.nvm/versions/node/v23.9.0/bin
      - /opt/homebrew/bin
      - /usr/bin
      - /bin
```

Profiles can append structured args to configured backend commands. Repowire does not hardcode provider model names.

`env_path` is optional. When set, Repowire injects it as the spawned agent's `PATH`
before launching the backend command. This is useful for durable job workers
started by the daemon, because they should not depend on whichever PATH the tmux
server happened to inherit. When `env_path` is omitted, Repowire captures the
user's login-shell PATH and injects that as a fallback.

## Common workflows

Spawn a peer from CLI:

```bash
repowire peer new ~/projects/project-a
repowire peer new ~/projects/project-b --backend codex --profile fast
```

Inside tmux, omitted `--circle` means the current tmux session or window,
according to `daemon.circle_boundary`. Outside tmux, pass `--circle`; there is no implicit `default` circle. The literal name
`default` remains valid when you choose it explicitly.

From an agent, use the `spawn_peer` MCP tool when the path is allowed.

Verify registration:

```bash
repowire peer list
```

The new peer appears after its runtime starts and registers. Codex registers
when App Server creates the thread; a spawn message is optional opening context,
not a registration seed.

Restart a resumable peer:

```bash
repowire peer restart <peer>
```

Dashboard spawn and backend controls use the same spawn configuration as CLI and MCP surfaces.

## Commands and API

- CLI: `repowire peer new`, `repowire peer restart`.
- MCP: `spawn_peer`, `kill_peer`.
- HTTP: spawn and session-control routes exposed by the daemon.
- Dashboard: spawn dialog and backend/profile controls.

## Limits

- Spawning is disabled until `daemon.spawn.allowed_paths` and backend commands are configured.
- The configured command must resolve to an executable on the daemon's configured `PATH` before a pane is opened.
- A path outside the allowed roots is rejected.
- Killing a tmux pane is allowed only when the daemon can prove the pane belongs to the target peer: Repowire spawn ownership, or live pane hook metadata whose `peer_id` matches the target.
- Externally attached peers without matching metadata cannot have their pane killed by Repowire.
- Restart does not fall back to a fresh spawn when resume data is missing or stale.

## Troubleshooting

- Spawn is refused: check `daemon.spawn.allowed_paths` and backend command configuration.
- Spawned Codex peer is not visible yet: check `repowire service status` and `~/.repowire/codex-bridge.log`.
- Restart fails before killing the pane: inspect the session binding and backend resume support.
- Kill is refused for an external pane: rehook/link the pane or retire it manually; destructive pane control requires proof.

## See also

- [Configuration: spawn](../../reference/configuration.md#daemonspawn)
- [CLI reference](../../reference/cli.md#repowire-peer)
- [MCP tools](../../reference/mcp-tools.md#spawn_peer)
- [Peer identity lifecycle](../../concepts/peer-identity-lifecycle.md)
