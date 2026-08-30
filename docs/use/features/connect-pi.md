# Pi

Pi integrates through a native TypeScript extension. It does not use shell hooks or tmux keystroke injection.

## What gets installed

`repowire setup` writes `~/.pi/agent/extensions/repowire.ts`, Pi's global auto-discovery path. The extension uses Pi's bundled `typebox` schema entry point and current extension lifecycle.

## Native behavior

The extension opens one WebSocket peer for each active root session and uses Pi APIs directly:

- `session_start` registers the current session and consumes any Repowire spawn hint.
- Idle inbound messages use `sendUserMessage`; messages arriving during a turn use Pi's `steer` delivery mode.
- `message_update` and `turn_end` stream and close tracked query responses.
- Open asks resurface at `turn_end` until acknowledged.
- `session_shutdown` closes every connection and rejects pending requests during quit, reload, new-session, resume, or fork transitions.
- Repowire tools are registered directly with Pi.

Tmux is still used when Repowire itself spawns or organizes a Pi process, but it is not Pi's message transport.

For a normal Pi process launched outside tmux, the extension reads the local daemon token from `~/.repowire/config.yaml`, pre-registers the pane-less identity over authenticated HTTP, and joins a stable `project-<hash>` circle derived from the canonical project path. Pi and OpenCode sessions launched for the same project therefore share a standalone circle without inventing a global default.

## Verify

```bash
repowire pi status
pi
```

After Pi starts, `repowire peer list` should show a `pi` peer for the active session. If it does not, confirm `~/.pi/agent/extensions/repowire.ts` exists, inspect Pi's startup output for `[repowire]` errors, and run `repowire setup` again.

## Related

- [Agent backends](../../concepts/agent-backends.md)
- [Transports](../../operate/transports.md)
- [Spawning](spawning.md)
