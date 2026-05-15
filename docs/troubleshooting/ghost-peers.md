# Ghost peers and stuck busy state

## Ghost peers

A peer shows `online` in `list_peers` but the agent process is gone (closed terminal, killed tmux pane, crashed runtime). Symptom: routing to it succeeds at the daemon but the agent never sees the message.

This happens when a peer exits without firing its `SessionEnd` / `Stop` hook — for example, a force-killed tmux pane, an OS-level kill, or a network partition during a remote session.

### Recovery

Ghosts are evicted by **lazy repair**: the next MCP tool call from any peer triggers `lazy_repair()` (at most once per 30 seconds), which checks every peer's `last_seen` against the staleness threshold and demotes anything past it. There is no polling thread that does this — see [lazy repair](../concepts/lazy-repair.md).

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

## Why no polling

Repowire deliberately has no heartbeat or watchdog thread. State catches up on the next routing call. If a fully idle mesh is leaving you with stale state for too long, that's a sign you should reach for `repowire peer prune` rather than ask repowire to poll.
