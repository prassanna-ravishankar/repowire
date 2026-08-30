# OpenCode

OpenCode integrates via a TypeScript plugin instead of shell hooks. The plugin holds a persistent WebSocket connection to the daemon for the lifetime of the session.

## What gets installed

| Path | What |
| --- | --- |
| `~/.config/opencode/plugins/repowire.ts` | Global plugin (default) |

The plugin uses the current OpenCode plugin API (`@opencode-ai/plugin`). It hooks into session lifecycle events from inside the OpenCode runtime — not via external shell hooks — and bridges them to the Repowire daemon over WebSocket. OpenCode calls the plugin's `dispose` hook on reload or shutdown so connections and pending requests are released without process-level signal handlers.

OpenCode's `session.status` event is the authoritative status source:
`busy` sends `status=busy, turn_state=working`, and `idle` sends
`status=idle, turn_state=idle`. Query timeouts also clear the plugin's local
busy flag so a failed injected prompt does not strand the peer.

## Why a plugin instead of hooks

OpenCode does not expose Claude-style stdout hooks. The plugin model gives the integration:

- Direct access to session state without parsing a transcript file.
- Persistent WebSocket connection (no spawn-on-each-event overhead).
- Tool-call interception in-process (used for the dashboard's tool-call detail view).

The trade-off: the plugin runs inside OpenCode's process, so a plugin crash takes the OpenCode session with it. Repowire's plugin is small and defensive on purpose.

## Verifying

```bash
repowire status
```

Open an OpenCode session, submit its first prompt, and watch `repowire peer list`. The peer registers from the first live session event. The plugin deliberately does not mark every historical session returned by `session.list()` online at startup. If the active peer does not appear, check OpenCode's plugin log for the error.

`repowire setup` installs globally in OpenCode's canonical plural `plugins` directory. It removes the old Repowire file from `~/.opencode/plugin/` after the new file is written, preventing the plugin from loading twice during migration.

Outside tmux, the plugin reads the local daemon token from `~/.repowire/config.yaml`, pre-registers its pane-less identity over authenticated HTTP, and uses the same stable project-derived circle as Pi. Inside a Repowire-spawned tmux pane, verified tmux/spawn placement remains authoritative.

## Troubleshooting

- Peer never appears → the plugin didn't load. Check that `~/.config/opencode/plugins/repowire.ts` exists and is non-empty; check OpenCode's log for plugin load errors.
- WebSocket errors in the plugin log → daemon is unreachable. See [Daemon unreachable](../../troubleshooting/daemon.md).
- Tool calls missing from the dashboard → confirm the plugin version matches the installed `repowire` package. Re-run `repowire setup` to refresh.
