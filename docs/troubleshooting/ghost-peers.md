# Ghost peers and stuck busy state

## Ghost peers

A peer shows `online` in `list_peers` but the agent process is gone (closed terminal, killed tmux pane, crashed runtime). Symptom: routing to it succeeds at the daemon but the agent never sees the message.

Ghosts should now be rare: a clean quit deregisters through the `SessionEnd` hook, and a killed agent is reported by its own ws-hook (`agent_exited`) within seconds. Both retire the peer so a leftover ws-hook cannot reconnect it back to life. A ghost that persists means *both* explicit tiers were missed — for example, the ws-hook itself was SIGKILLed along with the agent, or a network partition during a remote session.

### Recovery

Remaining ghosts are evicted by **lazy repair**: the next MCP tool call from any peer triggers `lazy_repair()` (at most once per 30 seconds), which checks runtime evidence and `last_seen`, and demotes connected pane peers after three consecutive honest `pane_alive=false` ping verdicts. There is no polling thread that does this — see [lazy repair](../concepts/lazy-repair.md).

Orphaned ws-hook *processes* (a hook outliving its agent, e.g. one installed before the agent-pid watcher existed) are swept once at daemon startup: the sweep kills hooks whose pane is gone, whose recorded agent pid is dead, or whose pane subtree contains only shells — and only on conclusive evidence. `repowire doctor` reports the current orphan count read-only; `repowire service restart` runs the sweep.

Startup also rehydrates live pane-backed peers whose daemon registry was lost
during the restart, but only when persisted peer identity and live pane metadata
prove the same `peer_id` or a valid daemon-minted birth certificate. Rehydrated
peers stay `offline` until their WebSocket hook reconnects; if no hook connects,
the daemon emits `startup_hydration_no_transport` instead of advertising the
peer as deliverable. Panes without that proof remain visible through
`/panes/orphans` and require explicit `repowire link`.

If you need an immediate eviction:

```bash
repowire peer prune
```

Removes all peers whose `last_seen` exceeds `daemon.prune_max_age_hours` (default 24h). For a faster cleanup, call any MCP tool from another peer — the next routing call will run lazy repair.

## Stuck `busy` state

A peer shows `busy` long after the turn that triggered it has ended. The most common causes:

1. **`Stop` / `AfterAgent` hook didn't fire.** The peer never marked itself `online` again. The next user prompt should reset it; if not, re-run `repowire setup` to rewrite the hook entries.
2. **Hook script error.** The hook ran but failed before reaching the status update. Check the hook log (visible directly in Gemini output; for Claude Code / Codex, look at `repowire serve` foreground output or the user-service log).
3. **`turn_state=awaiting_input`.** The peer is mid-turn waiting on user input (a permission prompt, a `read -p`, an MCP tool that suspended). This is not stuck — it's correctly reporting state. Send input to unblock it.
4. **`turn_state=pending_first_turn`.** A spawn-seeded peer whose seed message never reached the agent. Re-send via `notify_peer`.

Lazy repair also has a stale-state fallback for missed cancel/interrupt paths:
on the next normal routing or peer-list request, it resets peers that are still
`busy` with `turn_state=working` and have had no recent liveness progress for
longer than `daemon.stale_busy_timeout_seconds` (default 1800 seconds). It does
not touch `awaiting_input`, and it is not a guarantee that every backend emitted
a cancel event — it only reconciles stale daemon state without adding a polling
loop.

For Codex specifically, manual interrupt/Esc can abort the visible turn without
emitting a reliable `Stop`/cancel hook. In that case the daemon may continue to
show the peer as `busy` even though the TUI is ready for input again. This is a
known runtime-signal limitation, not a state Repowire should infer from timing
alone; lowering the stale-busy timeout too far can make long-running turns look
idle incorrectly. When Codex exposes a clean interrupt/cancel lifecycle event,
Repowire should use that instead of a heuristic.

## Why no polling

Repowire deliberately has no heartbeat or watchdog thread. State catches up on the next routing call. If a fully idle mesh is leaving you with stale state for too long, that's a sign you should reach for `repowire peer prune` rather than ask repowire to poll.
