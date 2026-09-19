# Peers and circles

## Peers

A peer is one running agent session. Runtime adapters—hooks, Codex App Server,
and plugins—normalize every supported agent into the same peer model. Peers have:

- a `name` (display name; auto-suffixed on collision: `repowire`, `repowire-2`),
- a `path` (working directory),
- a `circle`,
- a `status` (`online` / `busy` / `offline`),
- a free-form `description` the agent sets via `set_description`,
- a `backend` (`claude-code`, `codex`, `opencode`, or `pi`),
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

## Provenance

Backend says which runtime a peer is; provenance says how it reached the mesh and whether it can be addressed. The fields are orthogonal on purpose:

- `source`: `hook` for a runtime hook registration, `codex-app-server` for a thread the codex bridge found on the Codex App Server socket, `unknown` for peers registered before provenance existed. Unknown is a first-class value; the daemon only fills it from registration evidence, never by guessing.
- `initiator`: who opened the session: `user`, `agent` (a sub-agent spawned by another agent), or `system` (runtime machinery nobody opened, such as helper threads the ChatGPT desktop app creates). Empty when the runtime does not say.
- `parent_runtime_id` / `parent_peer_id`: a sub-agent thread's parent. The runtime id is stored; the peer id is resolved when read and is empty if the parent is not registered.
- `ephemeral`: the runtime does not persist the thread. Ephemeral threads can still accept input.
- `addressable`: the runtime's own verdict on direct input (Codex publishes `canAcceptDirectInput`). Codex multi-agent v2 sub-agent threads are usually `false` with `addressable_reason=subagent_direct_input_denied`, but a sub-agent the runtime says accepts input stays addressable. Non-addressable is inbound-only: the peer can still ack, reply, and notify.

Addressability is enforced, not just displayed: an ask or notify to a non-addressable peer is refused with `peer_not_addressable` before any tracker entry or queued delivery exists, broadcasts skip it, and a peer that loses addressability while live has its open inbound asks closed (askers are told) and its queued deliveries dropped. Outbound stays open: the peer can still ack, reply, notify, and broadcast.

`list_peers` and `repowire peer list` show only peers someone can send work to by default: addressable, not `system`, and with a live parent if they have one. The dashboard shows the full inventory, nests sub-agents under their parent, and dims the hidden ones with a badge (`no input`, `system`, or the runtime nickname). A denial observed after registration demotes the peer; only a fresh runtime verdict restores it.
