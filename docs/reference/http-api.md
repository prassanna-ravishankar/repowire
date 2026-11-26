# HTTP API

The daemon exposes HTTP routes for the dashboard, hooks, CLI helpers, and external integrations. The stable public client surfaces are the CLI and MCP tools; raw HTTP routes may move faster.

## Primary route groups

- `/health` and status routes for daemon checks.
- `/peers` for peer registration, listing, lookup, and lifecycle operations. Peer records include optional `model`, the last observed runtime model reported by the backend, and flattened provenance: `source` (`hook`, `codex-app-server`, `unknown`), `initiator` (`user`, `agent`, `system`, or absent), `parent_runtime_id`, `parent_peer_id` (resolved at read time, `null` when the parent is not registered), `ephemeral`, `addressable`, and `addressable_reason`. `GET /peers` is a complete inventory; `?source=`, `?addressable=true|false`, and `?listed=true|false` (the routing view's rule: addressable, not `system`, parent live if any) narrow it. `POST /peers` accepts an optional `provenance` object as a fresh classification (omitting it preserves what the daemon knows, and only an unknown `source` is inferred from `metadata.transport` / `hook_version`). `POST /peers/{id}/provenance` replaces a live peer's provenance and emits a `peer_updated` event; the codex bridge uses it to demote a thread whose direct input the App Server denied. A peer with `addressable=false` is inbound-only: `POST /ask` and `POST /notify` to it return **409** `{"error"|"reason": "peer_not_addressable", "peer_id", "parent_peer_id" (null when the parent is not registered), "hint"}` before anything is registered or queued, broadcasts skip it, and scheduled asks fail the same way. It can still ack, reply, notify, and broadcast. When a live peer loses addressability the daemon closes its open inbound asks with reason `peer_not_addressable`, drops its queued deliveries, notifies each asker, and records one `peer_not_addressable` event with the counts. Durable replay obeys the same gate: the WebSocket flush-on-connect and `GET /deliveries/pending` check the live verdict, replay nothing to a non-addressable peer, drop whatever is still queued (`dropped_not_addressable` in the drain response), and record the drop, so a row queued before demotion can never land after it. Includes `GET /peers/{id}/doctor` (read-only diagnostic report with contradiction detection) and `POST /peers/{id}/rehook` (non-destructive inbound ws-hook recovery, same-host, dry-run by default). `POST /peers/{id}/restart` (`{fresh, dry_run, message, wait, circle, from_peer}`; `wait` defaults to true) stops and relaunches the peer with resume and always answers 200 with a typed result (`outcome`, `failed.stage`, `would_run`, ...); only an unknown or ambiguous peer or missing spawn wiring is an HTTP error. `GET /peers/{id}/restart` returns the latest result; `DELETE /peers/{id}/restart` cancels an in-flight one. Every stage is journaled with its `restart_id` (`peer_restart_started`, `peer_restart_launched`, `peer_restart_restarted`, `peer_restart_failed`, `peer_restart_offline_ignored`); commands in results and the journal redact every value except the daemon-supplied resume argument, listed permission words, and model ids from the daemon's spawn config. `POST /peers/{id}/offline` accepts optional `pane_id` and `agent_pid`; a post whose pane and pid both match an execution a restart replaced is ignored (in memory, until the daemon restarts). `GET /peers/{id}` includes a `restart` block (restartability preview and last result).
- `POST /peers/{id}/offline` accepts an optional body `{reason, source, detail, terminal}` recording the truthful cause in the event log (e.g. `session_end`, `agent_exited`, `pane_takeover`). `terminal: true` retires the peer: its websocket is severed and reconnects claiming that peer_id are rejected (HTTP 409 / ws error `peer_retired`) unless they carry a live `agent_pid`. An operator close (`kill_peer`, `DELETE /peers/{name}`) is final: reconnects get HTTP 409 (`peer_closed` in the detail) or the ws error `peer_retired` with `final: true`, whatever proof they carry. An empty body keeps the legacy plain-offline behavior.
- `POST /peers/identity/validate` checks a runtime birth certificate and rehydrates its peer; `POST /peers/identity/renew` (`{birth_certificate, backend, path}`) exchanges a matching certificate that names a runtime session, expired or not, for a fresh one only while its peer is connected on that same session (409 otherwise). The Codex bridge renews long-lived threads this way.
- `GET /panes/orphans` lists local tmux panes not bound to any registered peer (with a display-only backend hint); `POST /panes/{pane_id}/link` adopts one — registers the peer AND establishes its inbound ws-hook, rolling the registration back if no live transport connects (fail-closed against ghosts). Driven by `repowire link`.
- `/ask`, `/ack`, `/decline`, and `/asks/pending` for ask lifecycle. `GET /asks/history?pane_id=|peer_id=&since=RFC3339` lists the closed ask threads a peer took part in (as asker or recipient) after `since`, read from the durable trace ledger rather than the in-memory tracker, plus the peer's `repowire_session_id`. Backs the git-trailer hook and `repowire why`. `POST /decline` closes an unhandled ask with `close_reason="declined"` and returns the reason to its asker.
- `POST /asks/{correlation_id}/delegate` (`{to_peer, note, from_peer, circle}`) hands an open ask to another peer, keeping its correlation id and asker. A `from_peer` that resolves to a registered peer must be the current recipient (403 `not_the_recipient`). Refusals: unknown target 404, the asker or current recipient 422, a circle or addressability rejection 403/409, a cross-machine ask or target 422, a live job's dispatch ask 409 `job_ask`, a closed ask 410, 8 hops 422. A delivery that fails (503) returns the ask to the delegator. The response is `{correlation_id, delegated_to, transport, message_id, delegations: [{from, to, note, at}]}`; `/asks/pending` and `/asks/{cid}/wait` include the same `delegations`. After delegation, `/ack`, `/decline`, and `/answer` from a former recipient return 409 `ask_delegated`, and two racing replies on one ask deliver once (the other gets 409 or 410).
- `POST /asks/{correlation_id}/wait` blocks (bounded, ~50s per call) until the ask is answered or acked, returning a resolution snapshot `{status, reply, outcome, close_reason, ...}` or `{status: "pending"}` on timeout with nothing recorded. First wait switches the ask to pull reply delivery so the reply is retained on the ask instead of injected at the asker. Backs the MCP `wait_on_ack` tool.
- `/answer` records a typed answer to a structured question; `POST /questions/ask-blocking` registers a blocking structured question (tool permission) and holds the connection open until it is answered or the daemon's wait cap elapses, then returns the typed answer (fail-closed: a tool-permission question denies on timeout). Used by blocking transports such as the ACP broker and the Claude Code `PreToolUse` approval hook.
- `/query` is a legacy blocking compatibility route. It opens a structured text ask through the ask lifecycle, waits for the recipient's answer/ack, and returns the old `{text, error, status}` response shape for existing clients.
- `/inbox` routes read and answer the [human inbox](#human-inbox).
- `/traces/{trace_id}` returns the recorded delivery stages for an ask (`correlation_id`) or notify (`delivery_id`) from the local delivery trace ledger.
- `/messages` reads agent peers' messages by id: `GET /messages?peer=P` lists P's messages newest first (`?unread=1`, `?limit=N`; previews, no bodies; with P's `delivery` mode), `GET /messages/{id}` returns one with its body and leaves it unread, and `POST /messages/{id}/read` returns it and marks it read. Either read takes `?peer=P` to answer only when P is the recipient. `POST /peers/{id}/message-delivery` (`{"mode": "push" | "pull"}`) sets a peer's delivery mode. `POST /notify`, `POST /ask`, and `POST /asks/{cid}/delegate` responses carry the recipient's `message_id`, and so do the `notify`/`ask` WebSocket frames and `GET /deliveries/pending` rows. The human's messages are `/inbox` instead.
- `GET /wire/threads` lists ask and notify threads, newest activity first, as `{threads, cursor}`. Filters: `circle`, `peer`, `kind`, `state`, `stuck_only=true`, `since` (`90m`, `24h`, `7d`), `limit` (default 50, at most 100), `cursor`. `GET /wire/threads/{thread_id}` returns one thread with its `stages`. A thread carries `protected` and `protected_by` (why), and `provenance`: each party as the stage that named it recorded it (`peer_id`, `display_name`, `circle`, `role`, `private`, `unknown`), oldest first. A party with `known: false` is gone from the daemon; its `display_name` is the last one its thread recorded, historical and not an address, since another peer may hold that name now. Both are metadata only (no message text) and sit behind the daemon token like `/traces` (so a local request passes when the token is not required); a bad `cursor` or `since` is 422. See [`wire_query`](mcp-tools.md#wire_query).
- `POST /wire/threads/{thread_id}/reassign` (`{to, reason}`), `/nudge` (`{note?}`) and `/cancel` (`{reason}`) are the operator's actions on an open ask (`rfcs/wire-actions.md`). They require the daemon token as `Authorization: Bearer`, with no other way in: no tokenless pass and no local-dashboard pass, and a daemon without a token answers 403. A reassign moves the ask to another peer (never back to a former holder), drops the old holder's queued copies, and tells the old holder and the asker. A cancel closes it, delivers the reason to the asker as the ask's outcome, and tells the holder. A nudge reminds the holder, at most once per 10 minutes per ask (429 otherwise). The reason or note is at most 200 characters. Each returns `{correlation_id, action, holder, notified, dropped_queued}`. Orchestrators take the same actions over MCP (`reassign`, `nudge`, `cancel_ask`), bound by the protected-thread and circle rules; the operator is not.
- `POST /peers/{name}/wire-visibility` (`{mode: "visible" | "private"}`) sets a peer's wire visibility (see [`set_wire_visibility`](mcp-tools.md#set_wire_visibility)). Operator only, with the same token rule as the actions; a peer sets its own over MCP.
- `/ws` for live delivery.
- `/schedules` for one-shot and recurring scheduled messages. `POST /schedules` resolves `to_peer` as a send would: **404** for a peer this daemon has never known (one known only by its durable record, offline, is accepted), **403** across a circle boundary, **409** for an ambiguous name or a target that takes no input. A scheduled send that fails when it fires is reported to its sender as a notify from `repowire:scheduler` and journaled as `schedule_failed`.
- `/jobs` / work routes for durable tracked work.
- `/spawn` creates tmux-backed peers (an optional `message` is held, for up to 30 minutes, and delivered as a notify from `from_peer` when the spawned runtime connects on its pane, with a notice to `from_peer` after 90 seconds; `seed_state` in the response reports `awaiting_registration` or `undeliverable`); `GET /spawn/config` reports whether spawning is enabled, configured commands/profiles, allowed paths, and the active `circle_boundary` (`session` or `window`). In window mode the CLI supplies its current pane internally so the daemon can split the peer into that window. `GET /swarm/snapshot` (`experiments.swarm_snapshot`; 404 naming the setting when off) returns the read-only swarm manifest described under [`repowire swarm`](cli.md#repowire-swarm). Restore's daemon half, under the same experiment, never touches tmux (the CLI does): `POST /swarm/plan` takes a manifest and returns each pane, in its place, as `launch` (the agent's registered path and its backend's resume command, with the permissions recorded at snapshot time) or `shell` with a `skip` reason. Agents that are online, closed by an operator, a live job's executor, whose session is bound to another peer, without a resumable session file, or with unmodeled permissions are never relaunched. The plan carries a `plan_id`, valid for 30 minutes in this daemon's memory. `POST /swarm/placed` (`{plan_id, pane_id, peer_id, tmux_session}`) records a pane the CLI launched as spawn records one, from the daemon's own mapping, and only for an issued plan's launch slot, once, with the pane in the session and window the plan gave it: an ownership record is kill and restart proof. `POST /swarm/verify` (`{placed}`; Codex is read from the daemon's own record of the backend) reports each agent as `back`, `twin` (another identity in its pane), `awaits_first_turn` (Codex) or `missing`; it never rebinds. `POST /peers/{id}/fork-backend` non-destructively starts an agent-role sibling on another configured backend in the source peer's project and circle. The source stays live and its backend-native conversation history is not copied.
- `/attachments` for upload and download.
- `/dashboard` for the static dashboard bundle.

## Jobs Execution Policy

`POST /jobs` accepts `process_scope` and `continuity` for path/backend durable
jobs. Unassigned path/backend jobs default to `process_scope=per_fire`, so each
run uses a daemon-spawned or backend-resumed executor process that is released
after terminal completion. Reused persistent executors and explicitly assigned
peers are not killed by per-fire cleanup. One-shot jobs default to
`continuity=fresh`; recurring jobs default to `continuity=resume`, so the next
fire resumes backend-native runtime context when a runtime session id is
available. Use `continuity=fresh` to avoid backend resume.

## Jobs State Semantics

Fire completion is structural: dispatch asks come from the built-in `@jobs`
service peer, the daemon arms the fire when the executor's prompt turn is
observed (`running` / phase `turn_started`), and the executor's next assistant
turn completes it with the final message as `result_summary`. Executor death
(terminal peer offline or daemon-restart reconcile) fails in-flight fires with
`executor_died`. Only the runner's `dispatching` phase is lease-bounded —
`delivered` fires are never reaped by a wall clock. Every state transition
emits a `job_state_changed` event, and terminal states best-effort notify the
owner (owner → creator → circle orchestrator). `PATCH /jobs/{id}` remains
available for progress enrichment and the explicit hold-open escape hatch.

## Jobs List Views

`GET /jobs` returns the full durable-work list by default. Dashboard-style clients that only need row data can use:

```http
GET /jobs?view=summary
```

The summary view keeps the same `{ "work": [...], "recurring": [...] }` envelope and preserves ids, state, timestamps, ownership/routing fields, result summaries, and trimmed execution target/delivery metadata. It omits heavier detail fields such as full requests, provenance, runner state, prompt bodies, and progress history. Fetch `GET /jobs/{id}/status` for the selected job or recurring `cal-*` template when full detail is needed.

## Auth

When `daemon.auth_token` is configured, clients send:

```http
Authorization: Bearer <token>
```

The dashboard served from the daemon's localhost origin is the exception: its
same-origin browser requests are accepted without exposing the token to JavaScript.

## Peer lifecycle

`DELETE /peers/{name-or-id}` is the explicit operator-close route used by the
dashboard's **Unregister peer** action and the CLI. It resolves the target to an
immutable `peer_id`, records a hard retirement tombstone, and removes it from the
registry. A still-running bridge cannot reclaim that closed identity.

`POST /peer/unregister` is intentionally non-terminal. Transport and service
clients use it to drop a registration before reconnecting the same session.

## Related

- [CLI](cli.md)
- [Operations: architecture](../operate/architecture.md)

## Human inbox

`POST /notify` and `POST /ask` accept a `name@machine` address for a peer on another machine in a shared circle ([circles across machines](../concepts/peers-and-circles.md#circles-across-machines)). A remote notify answers 200 with `delivery_state` `delivered`, `queued` (held for the peer's next turn), `uncertain` (outcome unknown, never resent) or `not_delivered` (the other machine refused it), and its `delivery_id`, which `/traces/{id}` shows; an unknown remote peer is 404 and attachments are 422. `POST /broadcast` adds `remote_sent`, `remote_queued`, `remote_failed` and `remote_uncertain` (`name@machine` lists), and `partial` (machines that did not answer, omitted when empty), when cross-machine circles are on and the sender is a registered peer in a shared circle; `exclude` and `bypass_circle` apply to local peers only.

Messages to the human peer (its name, `telegram`, or `slack`) are stored in the [human inbox](../concepts/human-inbox.md). `POST /notify` to it adds `inbox_item_id` and `surfaces` to the response. It also accepts an optional `correlation_id`, an ask between the sender and the human that the notify reports progress on. The item is filed under that ask's thread while the ask is open and the sender asked it or holds it, and the response then carries `thread_id`. Otherwise it is filed unthreaded and `thread_id` is omitted. The notify never answers, acks or closes the ask. A `correlation_id` on a notify to anyone else is **422** `thread_not_human`, and on a `name@machine` address it is 422; `POST /ask` to it adds `inbox: {item_id, surfaces}`. `POST /ask` accepts an optional `origin: {surface, ref}` naming the surface message an ask from the human was typed in, so the reply threads under it.

| Route | Purpose |
| --- | --- |
| `GET /inbox?unread=1&limit=N` | Inbound items, newest first, with per-surface `pushes` (`surface`, `status`, `surface_ref`). Returns `{human, human_peer_id, items, surfaces}` (`surfaces`: each surface's presence, with `quiet`, `quiet_until` and `held` under quiet hours). A push `status` is `pushed`, `failed`, `held` (waiting for the surface's quiet hours to end), `releasing` or `skipped` (released after the human had handled it). `surface=S` keeps items successfully pushed to S, listed in S's delivery order (newest rendered first, by numeric ref) rather than creation order; with it, `through_ref=N` keeps those whose first message on S has a numeric ref at or below N (Telegram message ids, which grow with every message in a chat). |
| `GET /inbox/{id}` | One item. |
| `POST /inbox/{id}/read` | `{surface}`. Marks the item read for every surface (idempotent). |
| `POST /inbox/{id}/reply` | `{surface, text, option_id?, ref?}`. On an open ask: acks it with `text` (or answers a structured question). Otherwise opens a new ask from the human to the item's sender. Returns `{action: acked\|answered\|asked\|read, correlation_id}`. An empty `text` on a non-ask only marks it read. |
| `POST /inbox/reply` | Same, finding the item by `{surface, surface_ref}` (for example a Telegram reply-to). 404 when no item was rendered as that ref. |

All routes return 503 when no human peer is registered.

Inbox surfaces connect over `/ws` with `capabilities: ["inbox_surface"]`. The daemon pushes `{type: "inbox_item", delivery_id, item, reply_to_ref}`. The surface answers with `{type: "delivery_ack", delivery_id, status: "pushed", surface_ref, surface_refs}` or `{..., status: "failed", error_class}`. `surface_ref` is the first message rendered and anchors later replies on the thread; `surface_refs` lists every message rendered (a long item is split), and a failed push still lists what was sent, so a reply to any of them finds the item. A bridge without the capability gets the item as a plain `notify`/`ask` frame, which proves only the socket write (`verified: false`).

The dashboard is a surface without a socket of its own. `GET /events/stream?surface=dashboard` marks the stream as the dashboard: the dashboard is live while one such stream is open, and a push to it is receipted (`surface_pushed`, `surface: dashboard`) once a stream has flushed the item's `inbox_item` event. The event stream carries `inbox_item` (`item_id`, `kind`, `from`, `from_peer_id`, `correlation_id`) and `inbox_updated` (`item_id`, `read`, `replied`, `surface`), never the text; the dashboard reads the item from `GET /inbox`. Relay share streams never carry inbox events.

## Pull-delivery asks

`POST /ask` accepts optional `reply_delivery: "pull"` (default `"push"`). Pull
mode is set before delivery so an immediate recipient reply is retained for
`POST /asks/{correlation_id}/wait`, whether the recipient acks or an ACP
runtime answers on the daemon's side (a runtime failure closes the ask as
`send_failed` with the error as its reply). Callers without a registered peer may list
their outstanding asks with `GET /asks/pending?peer_id=<sender>&direction=outbound`.
The wait endpoint still requires the original sender identity.
`GET /health` advertises `capabilities.ask_pull_delivery` when ask handling is wired.

## Targetless asks and routing

`to_peer` names the recipient. Under the default `routing.strategy: static`, an ask must name one. If the `to_peer` key is left out, the ask is refused with **400** `{"error": "no_target", "hint"}`, and nothing is registered or recorded.

When `routing.strategy` names an arm, an ask with no `to_peer` key is routed: the arm picks the recipient, and the pick is authorized like a named target. Optional `fallback_to` names the peer used if the arm fails or its pick is refused. A strategy never delivers to a service- or human-role peer unless `routing.allow_service_targets` is set. A `fallback_to` naming one returns **403** `{"error": "service_excluded", "peer", "role", "hint"}`. The response adds `routing: {arm, routed_to, routed_to_peer_id, confidence, fallback, fallback_reason}`. A routing failure with no `fallback_to` returns **503** `{"error": "router_unavailable", "arm", "reason", "hint"}`, and nothing is registered or delivered.

Only an absent key routes. `"to_peer": ""`, whitespace and `"to_peer": null` are named asks to nobody. They return **404** `Unknown peer: …` under every strategy, as before. Keys match case-insensitively, as JSON decoding always has, so `TO_PEER` names a target. A key that only resembles `to_peer` (for example `toPeer` or `to-peer`) is rejected with **400**, so a misspelled target never routes. A named ask is never rerouted, and responses to named asks carry no `routing` key. `@route` is an ordinary name. See [`routing`](configuration.md#routing).

The hosted relay has a separate [OAuth-authenticated MCP surface](relay-mcp.md);
it does not tunnel the daemon's administrative `/mcp` endpoint.
