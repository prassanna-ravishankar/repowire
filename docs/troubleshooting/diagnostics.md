# Diagnostic commands

When something is wrong, this is the order to run things.

## `repowire status`

```bash
repowire status
```

Shows:

- Which agent runtimes are detected.
- Which integrations (hooks, MCP, plugin) are installed for each.
- Whether the daemon is running on `127.0.0.1:8377` (or your configured port).
- Whether the relay connection is up (if enabled).
- Version of the installed `repowire` package.

First stop for any "did the install actually work?" question.

## `repowire doctor`

```bash
repowire doctor
```

Runs concrete health checks and includes daemon-reported channel/ACP broker
state when the daemon is reachable. The channel check reports whether the
experimental Claude Code channel is configured, whether `bun` is available, and
whether the token in `~/.claude.json` matches the daemon auth token. The ACP
broker check reports readiness, configured ACP peers, in-flight prompts, and
the last broker or permission-relay error seen by the daemon.

## `repowire peer list`

```bash
repowire peer list
```

God-view of the registry: every peer in every circle, including the calling shell. If a peer you expect to see is missing, the agent's hooks aren't reaching the daemon. If a peer is there but `offline`, the daemon heard about it once but hasn't seen it recently.

## `repowire serve` (foreground)

```bash
repowire serve
```

Stops the user-service daemon and runs the daemon in the foreground with logs to stderr. Re-run any failing operation in another shell and watch the log for the routing path. Ctrl-C to stop; the user service won't auto-restart while you've manually started a foreground daemon on the same port.

## `repowire peer prune`

```bash
repowire peer prune
```

Removes peers whose `last_seen` exceeds `daemon.prune_max_age_hours` (default 24h). Use when stale ghosts are cluttering `list_peers` and you don't want to wait for the next lazy-repair pass.

## Daemon logs

The user-service log location is reported by `repowire status`. On macOS this is typically under `~/Library/Logs/`; on Linux, under `journalctl --user -u repowire` for systemd installs.

## MCP shim and daemon logs

`repowire mcp` runs as a small stdio proxy inside each agent. It reports
connection/auth/proxy failures to the runtime's MCP stderr; tool execution and
routing errors are owned by the daemon log:

- **Claude Code** — `~/Library/Logs/Claude/mcp-server-repowire.log` on macOS.
- **Codex** — visible in the Codex log output.
- **OpenCode** — the plugin log inside OpenCode.

## Quick "is anything broken?" pass

```bash
repowire status && repowire peer list
```

If both are clean, the install is healthy. From there, route a test ask between two peers (see [first ask](../start/first-ask.md)) to confirm the end-to-end path.
