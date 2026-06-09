# Peer identity lifecycle

Peer identity is the daemon contract that lets Repowire route messages without guessing. It is deliberately stronger than a display name: display names are for humans, while `peer_id` is the immutable routing key for a registered peer.

## Identity fields

A live peer has these identity and lifecycle fields:

- `peer_id` — daemon-assigned stable id for routing and ask ownership.
- `display_name` / `name` — human-facing name. It can collide across circles, so it is not globally unique.
- `circle` — routing scope. Peers normally message peers in the same circle unless the caller passes an explicit circle or has a role that bypasses circles.
- `backend` — agent runtime, such as `claude-code`, `codex`, `gemini`, `antigravity`, `opencode`, or `pi`.
- `path` — working directory used for name allocation, filtering, and operator context. It is not sufficient by itself to prove peer identity.
- `pane_id` / WebSocket binding — local delivery endpoint for hook-based peers.
- `role` — `agent` by default; service, human, and orchestrator roles can bypass normal circle visibility.
- `description` — short task state set by `set_description` and shown in peer lists.
- `status`, `turn_state`, and `last_seen` — liveness and per-turn observability.

The in-memory `Peer` is the live routing record. The durable `SessionMapping` preserves fields that should survive daemon restart, including display name, circle, backend, path, role, description, and the last known agent pid when available. Those mappings live in `~/.repowire/state.db`; legacy `sessions.json` is imported once and then left untouched for downgrade/export compatibility.

## Source-of-truth hierarchy

Peer identity should be resolved from the strongest daemon-owned proof available
and should fail closed when only ambiguous hints remain. The intended authority
order is:

| Level | Signal | Authority | Notes |
| --- | --- | --- | --- |
| 1 | Daemon-minted `peer_id` in the live registry | Authoritative routing identity | Exact key for asks, acks, notifications, and peer lookup. |
| 2 | Daemon-minted runtime identity envelope | Authoritative handoff proof | Birth certificate minted during SessionStart registration so MCP can adopt a peer identity without path search. |
| 3 | Local pane runtime metadata plus process proof | Local adoption proof | Valid only when backend, daemon peer id, and owning agent process match the current MCP process. |
| 4 | Durable `SessionMapping` / session binding records | Compatibility and provenance | Useful for restoring circle, role, description, runtime session pointers, and history relationships; not pane ownership proof. |
| 5 | Durable spawn ownership, or live pane metadata with matching `peer_id` | Destructive-action proof | Authorizes kill/restart of a verified pane. Path/cwd alone is not destructive proof. |
| 6 | Display name, path, backend, machine, `last_seen` | Hints and filters only | Never sufficient by themselves to impersonate a peer or choose among ambiguous candidates. |

This hierarchy keeps three concerns separate:

- Routing identity: the daemon `peer_id` names the live peer.
- Runtime/session provenance: runtime session ids and session bindings explain
  where history and control state came from.
- Pane ownership: spawn ownership records or matching pane `peer_id` metadata
  decide whether Repowire may kill or restart a tmux pane.

Project path is deliberately downgraded to metadata. It is useful for display
name allocation, filtering, history discovery, and human context, but path is
not identity proof. A same-path peer may be a second terminal, a sibling runtime,
an inherited environment, or a stale process.

## Registration and reconnect

On registration, the daemon allocates a `peer_id` and builds a display name from the working directory and backend. If another active peer in the same circle already holds the display name, the daemon suffixes the new name. Offline same-name peers may be pruned so a fresh session can reclaim the name cleanly.

During hook-based `SessionStart`, the agent receives a compact self-identity context block from the daemon's effective peer record. That block includes the display name, peer id, circle, backend, role, project path, and branch when known. It is intentionally based on the daemon's `/peers` view, not only local tmux or spawn-hint guesses, so restored circle and role state are visible to the session.

A reconnect may reclaim an existing `peer_id` only when the claim still describes the same peer identity. Today that check is intentionally narrow and v0.13-compatible:

- backend must match;
- path must match when both the existing peer and the reconnect claim provide one;
- pane ownership is updated only after that identity check passes.

If the check fails, the stale `peer_id` claim is ignored and the daemon allocates or adopts a different identity. This prevents an old environment variable or stale pane metadata from binding a different session's WebSocket to the wrong peer.

Orchestrator reconnects get one additional bounded repair path because they are long-lived control peers. If an orchestrator hook reconnects without a `peer_id`, the daemon may adopt exactly one offline peer in the same circle with the same canonical display name, backend, role, and project path. That preserves queued deliveries across daemon restarts or WebSocket churn without allowing general path-based identity takeover.

## Linking an orphan pane (link vs spawn)

When an agent is already running in a local tmux pane but never registered — hooks or MCP did not fire — the daemon cannot see it. `GET /panes/orphans` lists every unregistered local pane (with a display-only backend hint), and `repowire link --pane %NN --backend X` adopts one intentionally.

Link is distinct from spawn: **spawn** starts a *new* agent in a working directory; **link** adopts an *existing* running agent the daemon missed. Link is fail-closed against ghosts — it registers the peer and then establishes the inbound ws-hook, and reports success only when a live WebSocket connection is observed. If the transport cannot be established, the registration is rolled back so no transportless peer is left in the roster, and a retry hint is returned. Linking is same-host only (the daemon cannot spawn a hook into a foreign pane). A linked peer has no runtime session id until its hooks report one, so resume stays unavailable until then — the daemon does not fabricate it.

When a new peer claims a pane already held by another peer, the old peer normally loses that pane binding and is marked offline. Pane lookup should then resolve to the current live owner, not a zombie registration.

## Retirement

Some offlines are *terminal*: the caller knows the agent behind the peer is gone (a `SessionEnd` quit, the ws-hook reporting its agent pid exited, a pane takeover, or three consecutive honest `pane_alive=false` ping verdicts). A terminal offline retires the peer_id: the daemon severs its websocket and rejects any reconnect claiming that id unless the claim carries a *live* `agent_pid`. This is what stops an orphaned ws-hook's reconnect loop from resurrecting a dead peer. A fresh `SessionStart` always carries a live agent pid and therefore reclaims the identity cleanly; retirement records are in-memory and pruned after ~72 hours (after a daemon restart, the ping backstop re-converges on any orphan that re-registers).

There is one protected case: a fresh live `orchestrator` peer keeps sticky ownership of its tmux pane. If a temporary same-pane session starts in a split terminal, the daemon can register that session without assigning the pane (`pane_assigned=false`). The temporary peer can still use outbound MCP/HTTP tools, but it does not get inbound hook transport and must not clear the incumbent orchestrator's pane metadata or WebSocket hook.

Orchestrator role repair follows the same boundary. `claim_orchestrator_role`
and `repowire peer claim-role orchestrator` can repair stale, offline, or
mapping-only holders, but they cannot demote a fresh online/busy orchestrator
holder. To intentionally replace a live orchestrator, stop that holder first so
the daemon no longer treats it as fresh.

MCP lazy registration treats tmux pane lookup as a locator, not as identity proof. A by-pane daemon result is accepted only when local pane runtime metadata proves the same daemon peer id, backend, and owning agent process. If that proof is missing or belongs to another process, MCP registers or resolves its own peer instead of adopting the incumbent. This keeps path and display name as useful context while avoiding path-based identity takeover.

MCP lazy registration checks a daemon-minted birth certificate before the
remaining compatibility fallback. That fallback is path+backend lookup when MCP
starts without pane context, no local metadata is available, and exactly one
online candidate matches. This is a narrow bridge for stripped tmux
environments, not an identity source of truth.

## Runtime birth certificates

Hook-based `SessionStart` registration mints a short-lived runtime identity
envelope after daemon registration. The envelope is persisted in SQLite with an
unguessable nonce and written into local hook metadata where the MCP server can
read it.

Current envelope fields:

- `peer_id`
- `display_name`
- `backend`
- `project_path`
- `runtime_session_id` or hook `session_id` when known
- `pane_id` when the runtime is pane-local
- `agent_pid` / process proof when available
- `issued_at` and `expires_at`
- an unguessable nonce
- small metadata such as circle and role

MCP lazy registration validates this envelope through the daemon before
path+backend lookup. Validation rejects expired envelopes, backend mismatches,
pane reuse, process mismatches, and envelope fields that do not match persisted
daemon state. If the daemon restarted and no in-memory peer exists, a valid
certificate can rehydrate the peer from persisted identity evidence. When the
envelope is absent or invalid, MCP may register a fresh peer or fail closed for
strict outbound tools; it should not silently adopt identity from path alone.

Birth certificates do not make the daemon the owner of raw transcript history
and do not authorize destructive pane actions. Runtime session ids remain source
history/provenance. Destructive controls still require spawn ownership or live
pane metadata that names the target `peer_id`.

## Display-name ambiguity

Display names are scoped and human-facing. A name can exist in multiple circles, so routing code must not silently choose among multiple viable matches.

Use `peer_id` when exact routing matters. If you use a display name and more than one live candidate matches without an explicit `circle=`, the daemon returns a conflict instead of guessing. Passing `circle="name"` narrows the lookup. MCP `list_peers` and `repowire peer describe` expose both display names and peer ids so operators can disambiguate.

## Descriptions and stale task state

`description` is intentionally lightweight: it is task state, not durable truth. Agents should call `set_description("brief task summary")` when starting work and clear or replace it when focus changes.

Because agents can forget to clear it, the daemon bounds stale descriptions with a clear-on-read TTL (`daemon.description_ttl_seconds`, default 900 seconds). There is no polling loop. When a peer is read through `/peers` or peer lookup and its description is older than the TTL, the daemon clears it in memory and in the durable mapping. A description restored from durable mapping state gets stamped on first read so it has a bounded TTL window rather than living forever.

## `last_seen` and liveness

`last_seen` is the daemon's most recent activity timestamp for the peer. It is refreshed by registration, reconnect, status updates, description updates, role claims, and MCP `touch` calls. MCP tools touch on entry so outbound tool activity refreshes the liveness clock even if an inbound WebSocket hook dropped, but touch does not change the peer's transport status.

Runtime/session liveness is separate from transport/socket liveness. A hook-based peer may have a live tmux pane and agent process while its background WebSocket sidecar is disconnected; in that state the peer can remain `online` or `busy`, but ask/notify delivery over the WebSocket still fails explicitly because inbound transport is unavailable. Lazy repair only demotes disconnected pane-backed peers when runtime evidence is also missing: the recorded agent PID is not alive and tmux no longer shows the pane. Fresh HTTP pane pre-registration starts `offline` until a WebSocket connection or status update promotes it.

`last_seen` is observability, not a unique identity selector. Repowire avoids choosing between ambiguous display-name matches based only on recency because that can misroute work.

## Routing observability

Routing events and logs should make misroutes diagnosable without adding ad hoc logs. For asks and notifications, the daemon records human names and resolved ids in events:

- `from` / `to`
- `from_peer_id` / `to_peer_id`
- delivery status where applicable
- ask `correlation_id`

The WebSocket router also logs the intended recipient name, resolved peer id, frame `to_peer`, and delivered pane id for ask/notify sends. Those fields distinguish a caller typo, an ambiguous display-name lookup, a stale registry entry, and a transport binding problem.

## Compatibility boundary

This contract describes current v0.13 behavior. It does not make every route transport-neutral, persist `last_seen` across restart, or replace peers with sessions as the public API. The session-native architecture remains incremental: sessions are becoming the durable unit of work, while peers remain the live runtime executors.

## See also

- [Peers and circles](peers-and-circles.md)
- [Message types](message-types.md)
- [Lazy repair](lazy-repair.md)
- [MCP tools: `list_peers`](../reference/mcp-tools.md#list_peers)
- [CLI: `repowire peer describe`](../reference/cli.md#repowire-peer)
