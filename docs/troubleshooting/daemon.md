# Daemon unreachable

Symptoms: MCP tools return errors, `repowire peer list` is empty even when agents are running, the dashboard won't load.

## Is the daemon running?

```bash
repowire status
```

Looks for the daemon on `127.0.0.1:8377`. If it's down, start it:

```bash
repowire serve
```

That runs the daemon in the foreground. Useful for seeing errors directly. The installed user service (launchd on macOS, systemd on Linux) should auto-restart the daemon at login; if it doesn't, re-run `repowire setup` to reinstall the service.

## Wrong port?

If you changed `daemon.port` in `~/.repowire/config.yaml`, every hook still expects `8377` unless you've also reinstalled them. Either revert the port, or re-run `repowire setup` after the change.

## Auth token mismatch

If `daemon.auth_token` is set in `~/.repowire/config.yaml`, the daemon requires a `Bearer` token on WebSocket connections. Hooks and the MCP server read the same config and present the token, so this only breaks for external callers (your own scripts hitting the daemon). Confirm those callers send `Authorization: Bearer <token>`.

## CORS or origin rejected

Browser-side errors that mention origin or CORS: the daemon restricts CORS to localhost origins (plus `repowire.io` when the relay is enabled). If you're hosting the dashboard somewhere else, that origin needs to be added explicitly — or front the daemon with a proxy you control.

## Logs

```bash
repowire serve  # foreground, all logs to stderr
```

Or check the user-service log location reported by `repowire status`. Look for stack traces from FastAPI; routing errors are usually visible in the first 20 lines.

## Process hanging on port 8377

Something else is bound to the port:

```bash
lsof -i :8377
```

Kill the offending process or change `daemon.port` (and re-run `repowire setup`).
