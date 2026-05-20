# OpenCode

OpenCode integrates via a TypeScript plugin instead of shell hooks. The plugin holds a persistent WebSocket connection to the daemon for the lifetime of the session.

## What gets installed

| Path | What |
| --- | --- |
| `~/.opencode/plugin/repowire.ts` | Global plugin (default) |
| `.opencode/plugin/repowire.ts` | Local plugin (when installed per-project) |

The plugin uses the OpenCode plugin API (`@opencode-ai/plugin`). It hooks into session lifecycle events from inside the OpenCode runtime — not via external shell hooks — and bridges them to the repowire daemon over WebSocket.

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

Open an OpenCode session and watch `repowire peer list`. The peer registers on session start over the WebSocket. If it doesn't appear, the plugin failed to load — check OpenCode's plugin log for the error.

## Global vs local

By default `repowire setup` installs the plugin globally at `~/.opencode/plugin/`. You can install it per-project with the `--local` flag on the OpenCode installer step, which writes to `.opencode/plugin/` in the current directory. Use local installs when one project on a machine wants a different plugin version than the rest.

## Troubleshooting

- Peer never appears → the plugin didn't load. Check that `~/.opencode/plugin/repowire.ts` exists and is non-empty; check OpenCode's log for plugin load errors.
- WebSocket errors in the plugin log → daemon is unreachable. See [Daemon unreachable](../troubleshooting/daemon.md).
- Tool calls missing from the dashboard → confirm the plugin version matches the installed `repowire` package. Re-run `repowire setup` to refresh.
