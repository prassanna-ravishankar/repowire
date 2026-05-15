# Peers and circles

## Peers

A peer is one running agent session. Claude Code, Codex, Gemini CLI, and OpenCode all register as peers through the same hooks pattern. Peers have:

- a `name` (display name; auto-suffixed on collision: `repowire`, `repowire-2`),
- a `path` (working directory),
- a `circle`,
- a `status` (`online` / `busy` / `offline`),
- a free-form `description` the agent sets via `set_description`,
- a `backend` (`claude-code`, `codex`, `gemini`, `opencode`, …),
- a `last_seen` timestamp,
- and a `turn_state` (`idle`, `working`, `awaiting_input`, `pending_first_turn`, or empty when unknown).

Peer state lives in the local daemon at `127.0.0.1:8377`. It is not synced anywhere by default. Liveness is repaired lazily on the next MCP call rather than by a polling loop — see [lazy repair](lazy-repair.md).

## Circles

A circle is a logical subnet. Peers can only message peers in the same circle unless you pass an explicit `circle=` argument that targets a different one. Circles map to tmux sessions by default, so opening agents in the same tmux session puts them in the same circle.

Use circles to keep work-domain peers from talking to home-project peers when you don't want them to. They are scoping, not authorization — a peer with knowledge of a name and circle can reach it.

## Roles

Most peers run as `agent`. A peer can also register as `orchestrator` — same routing, different lifecycle expectations. The [orchestrator pattern](orchestrator.md) covers when to set one up.

## Listing peers

The MCP `list_peers` tool returns peers across all circles by default, filtered to `online` + `busy` status, with the calling peer hidden. Pass `show_offline=True` or `include_self=True` to widen the view. The CLI `repowire peer list` returns the god-view: every peer in every circle, caller included.
