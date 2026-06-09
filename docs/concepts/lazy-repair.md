# Lazy repair

Repowire avoids polling. Liveness checks, persistence, and ghost eviction run at most once per 30 seconds, and only when an MCP tool is already being handled. Disk writes are debounced via dirty flags and flushed on the same trigger or on daemon shutdown.

The practical consequence: a fully idle mesh consumes near-zero CPU. Peers do not heartbeat. State catches up the moment something happens.

## Why this matters

The mesh is a routing hub, not a monitoring system. If you want certainty about a peer's state, address the peer — its `Stop` hook will refresh `last_seen` on its way out, and the next MCP call from anywhere will run `lazy_repair()` and reconcile.

The corollary: `list_peers` results are *eventually consistent*. A peer that crashed without a clean `Stop` may still show `online` for up to a minute until the next lazy repair sees missing runtime evidence or stale `last_seen` and demotes it. A peer whose WebSocket hook disconnected but whose tmux pane or agent process is still present may remain `online`/`busy`; delivery over that missing inbound transport still fails explicitly.

Lazy repair is the *backstop*, not the primary deregistration path. Clean quits deregister explicitly through the SessionEnd hook, and a crashed agent is reported by its own ws-hook (`agent_exited`) within seconds — both mark the peer offline terminally. Repair pings only catch what those tiers miss.

For connected pane-backed peers, the liveness ping is tri-state: the hook answers `pane_alive: true` (agent present), `pane_alive: false` (tmux/ps answered authoritatively and the agent is gone), or omits the field entirely when the check itself failed — a tmux or ps hiccup says nothing about the pane. The daemon demotes only after **three consecutive** honest `false` verdicts (the same strike limit the hook applies before exiting), and the demotion is terminal: the peer is retired and its reporting hook cannot reconnect it back to life. Inconclusive results neither strike nor reset.

## What this rules out

- No periodic heartbeat from agents.
- No `setInterval`-style background polls in the daemon.
- No "watchdog" thread for ghost peers.
- No eager disk writes on every state change.

If you find yourself wanting one of these, the right answer is almost always to piggy-back the work on the next MCP call.
