# Lazy repair

Repowire avoids polling. Liveness checks, persistence, and ghost eviction run at most once per 30 seconds, and only when an MCP tool is already being handled. Disk writes are debounced via dirty flags and flushed on the same trigger or on daemon shutdown.

The practical consequence: a fully idle mesh consumes near-zero CPU. Peers do not heartbeat. State catches up the moment something happens.

## Why this matters

The mesh is a routing hub, not a monitoring system. If you want certainty about a peer's state, address the peer — its `Stop` hook will refresh `last_seen` on its way out, and the next MCP call from anywhere will run `lazy_repair()` and reconcile.

The corollary: `list_peers` results are *eventually consistent*. A peer that crashed without a clean `Stop` may still show `online` for up to a minute until the next lazy repair sees missing runtime evidence or stale `last_seen` and demotes it. A peer whose WebSocket hook disconnected but whose tmux pane or agent process is still present may remain `online`/`busy`; delivery over that missing inbound transport still fails explicitly.

## What this rules out

- No periodic heartbeat from agents.
- No `setInterval`-style background polls in the daemon.
- No "watchdog" thread for ghost peers.
- No eager disk writes on every state change.

If you find yourself wanting one of these, the right answer is almost always to piggy-back the work on the next MCP call.
