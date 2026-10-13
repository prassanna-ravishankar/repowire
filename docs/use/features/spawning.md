# Spawning

## What it is

Spawning starts or restarts agent peers from configured backend commands and allowed project paths. It is the daemon-controlled alternative to opening another terminal manually.

## When to use it

Use spawning when an orchestrator, dashboard, CLI command, or MCP tool should launch another peer in a known project folder. Use manual launch when you need ad hoc terminal control outside the configured allowed paths.

Use restart to stop a peer and relaunch it with backend resume in a new pane of the same window, keeping its `peer_id`, circle, and role. Restart reconstructs the launch from the backend template, cwd, permission mode, and resume id, defaults whatever is missing (listed in the result), and fails with a typed stage and the exact command to run by hand. A peer can restart itself.

Use dashboard `Fork to…` when you want a second backend to join the same project and circle without stopping the source peer. A fork shares the working tree, not the source runtime's private conversation history, so coordinate before both peers edit overlapping files.

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

Restart a peer (resumes when its session file exists, otherwise a fresh launch):

```bash
repowire peer restart <peer> --dry-run   # show the reconstructed launch
repowire peer restart <peer>
repowire peer restart <peer> --status    # latest result
```

Dashboard spawn and backend controls use the same spawn configuration as CLI and MCP surfaces.

## Commands and API

- CLI: `repowire peer new`, `repowire peer restart`.
- MCP: `spawn_peer`, `kill_peer`, `restart_peer`.
- HTTP: `POST /spawn`, `POST /peers/{peer_id}/fork-backend`, and session-control routes exposed by the daemon.
- Dashboard: spawn dialog, backend/profile controls, non-destructive `Fork to…`, and **Restart peer** beside **Unregister peer** in a peer header.

## Limits

- Spawning is disabled until `daemon.spawn.allowed_paths` and backend commands are configured.
- The configured command must resolve to an executable on the daemon's configured `PATH` before a pane is opened.
- A path outside the allowed roots is rejected.
- Killing a tmux pane is allowed only when the daemon can prove the pane belongs to the target peer: Repowire spawn ownership, or live pane hook metadata whose `peer_id` matches the target.
- Externally attached peers without matching metadata cannot have their pane killed by Repowire.
- Restart falls back to a fresh launch when the resume id is missing or its session file is absent after exit, and says so (`resume_mode=fresh`, `resume_reason`).
- Restart sends SIGTERM and waits up to 20s; a runtime that has not exited by then is reported and nothing is relaunched. Restart does not send SIGKILL.
- Restart rebuilds only the permission arguments it models; with any other permission argument it relaunches the observed argv itself (`argv-verbatim`). With nothing observed or recorded it adds no permission arguments. See [`repowire peer restart`](../../reference/cli.md#repowire-peer).
- A peer whose hook predates hook version 2 is refused (`hook_too_old`); relaunch it once by hand on the current binary.
- Deliveries sent while a peer is between runtimes are queued and replay to the replacement; deliveries in flight at the moment of exit may be reported delivered without being read (#472). A runtime that has not registered within 90s reports a `register` failure (not a manual relaunch) even if it is still starting; check the new pane before doing anything by hand.
- Backend fork preserves the source peer and starts the target as an agent-role sibling; it does not copy backend-native conversation history.

## Troubleshooting

- Spawn is refused: check `daemon.spawn.allowed_paths` and backend command configuration.
- Spawned Codex peer is not visible yet: check `repowire service status` and `~/.repowire/codex-bridge.log`.
- Restart failed: `repowire peer restart <peer> --status` shows `failed.stage`, `failed.last_known`, and `would_run.manual`, the command to finish by hand.
- Kill is refused for an external pane: rehook/link the pane or retire it manually; destructive pane control requires proof.

## See also

- [Configuration: spawn](../../reference/configuration.md#daemonspawn)
- [CLI reference](../../reference/cli.md#repowire-peer)
- [MCP tools](../../reference/mcp-tools.md#spawn_peer)
- [Peer identity lifecycle](../../concepts/peer-identity-lifecycle.md)
