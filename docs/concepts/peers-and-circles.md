# Peers and circles

## Peers

A peer is one running agent session. Runtime adapters—hooks, Codex App Server,
and plugins—normalize every supported agent into the same peer model. Peers have:

- a `name` (display name; auto-suffixed on collision: `repowire`, `repowire-2`),
- a `path` (working directory),
- a `circle`,
- a `status` (`online` / `busy` / `offline`),
- a free-form `description` the agent sets via `set_description`,
- a `backend` (`claude-code`, `codex`, `gemini`, `antigravity`, `opencode`, …),
- a `last_seen` timestamp,
- and a `turn_state` (`idle`, `working`, `awaiting_input`, `pending_first_turn`, or empty when unknown).

Peer state lives in the local daemon at `127.0.0.1:8377`. It is not synced anywhere by default. Liveness is repaired lazily on the next MCP call rather than by a polling loop — see [lazy repair](lazy-repair.md).

## Circles

A circle is a logical subnet. Ordinary agents communicate and spawn within their own circle; circle-bypassing roles such as orchestrators, services, and human surfaces can address others explicitly. By default circles map to tmux sessions, so agents in the same tmux session share a circle. Set `daemon.circle_boundary: window` to scope them to a tmux window instead; all panes in a window share its stable `window-N` circle. A runtime cannot move itself to another circle; choose the target from the CLI/orchestrator spawn surface or recreate it under the desired tmux boundary.

Use circles to keep work-domain peers from talking to home-project peers when you don't want them to. They are an agent-routing boundary, not a security boundary against local administrators or circle-bypassing roles.

## Roles

Most peers run as `agent`. A peer can also register as `orchestrator` — same routing, different lifecycle expectations. The [orchestrator pattern](orchestrator.md) covers when to set one up.

## Listing peers

The MCP `list_peers` tool returns peers in the caller's circle by default, filtered to `online` + `busy` status, with the calling peer hidden. Peers whose role bypasses circles — `orchestrator`, `service`, and human surfaces like `@telegram` / `@dashboard` / `@slack` — are always visible regardless of the caller's circle. Pass `circle="*"` to widen to the whole mesh, `circle="<name>"` to scope to a specific circle, `show_offline=True` for offline peers, or `include_self=True` to include the caller's own row. Orchestrator-role callers default to mesh-wide (`*`).

The CLI `repowire peer list` is god-view: every peer in every circle, caller included, regardless of role.
