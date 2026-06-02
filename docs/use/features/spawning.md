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
```

Profiles can append structured args to configured backend commands. Repowire does not hardcode provider model names.

## Common workflows

Spawn a peer from CLI:

```bash
repowire peer new ~/projects/project-a
repowire peer new ~/projects/project-b --backend codex --profile fast
```

From an agent, use the `spawn_peer` MCP tool when the path is allowed.

Verify registration:

```bash
repowire peer list
```

The new peer appears after its runtime starts and registers. Codex may register after its first interaction; spawn seed messages are used to trigger that first turn.

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
- A path outside the allowed roots is rejected.
- Killing a tmux pane is allowed only when the daemon can prove the pane belongs to the target peer: Repowire spawn ownership, or live pane hook metadata whose `peer_id` matches the target.
- Externally attached peers without matching metadata cannot have their pane killed by Repowire.
- Restart does not fall back to a fresh spawn when resume data is missing or stale.

## Troubleshooting

- Spawn is refused: check `daemon.spawn.allowed_paths` and backend command configuration.
- Spawned Codex peer is not visible yet: send or confirm the seed prompt so Codex fires `SessionStart`.
- Restart fails before killing the pane: inspect the session binding and backend resume support.
- Kill is refused for an external pane: rehook/link the pane or retire it manually; destructive pane control requires proof.

## See also

- [Configuration: spawn](../../reference/configuration.md#daemonspawn)
- [CLI reference](../../reference/cli.md#repowire-peer)
- [MCP tools](../../reference/mcp-tools.md#spawn_peer)
- [Peer identity lifecycle](../../concepts/peer-identity-lifecycle.md)
