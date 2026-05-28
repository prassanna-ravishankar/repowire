# Registry / Route Responsibility Split Plan

Project #2 item: `Architecture: split peer registry and route responsibilities`

Status: architecture-first audit plus incremental implementation notes.

## Current Coupling

`repowire/daemon/peer_registry.py` is currently the shared hub for several separate responsibilities:

- Peer identity and live state: `_peers`, lookup by id/name/pane, ambiguity handling, status, turn state, descriptions, role claims.
- Session mapping persistence: `SessionMapping`, `_mappings`, load/persist, pruning, durable circle/name/role/description sync.
- Circle authorization: sender/target resolution and bypass rules.
- Delivery-facing route service: `query`, `notify`, `deliver_ask`, and `broadcast`.
- Dashboard event buffer: `_events`, event persistence, SSE subscriber wakeups, event mutation.
- Liveness and repair: lazy repair, active repair, ghost demotion, stale reaping, transport disconnects, ask-stash loss events.
- ACP reply redelivery support: identity tuple matching, pending reply redelivery, pending-reply-loss event emission.

The route handlers mostly act as thin HTTP adapters, but they still reach into these mixed registry surfaces:

- `routes/peers.py` owns HTTP shape/validation but directly drives identity lifecycle, descriptions, role claim, transcript lookup, and peer-local MCP config operations.
- `routes/messages.py` mixes route validation with legacy `/query`, notify transport selection, broadcast, session status updates, legacy `/response`, chat event ingestion, and SSE streaming.
- `routes/asks.py` has the best current split for v0.13: route-level ask lifecycle and ACP completion live there, while transport choice is delegated to `transport_router.py`. It still calls registry methods for identity resolution, circle checks, notify-based ack delivery, and pending reply rebind helpers.
- `routes/websocket.py` handles protocol validation and still performs registration/status/name/circle mutations directly against the registry.

The v0.13 train already introduced `repowire/daemon/transport_router.py`, which is the right direction for ask/notify transport choice. The safest next work should extend that boundary without changing the public HTTP, MCP, hook, or WebSocket contracts.

## Target Module Boundaries

Keep `PeerRegistry` as the owner of peer identity and live state:

- Owns `_peers`, lookup semantics, ambiguity errors, circle-visible peer lists, pane ownership, status/turn-state mutation, descriptions, role claims, orchestrator liveness, and session mapping synchronization.
- Does not own route-level response mapping or message delivery orchestration after the split.

Add small modules around it, in this order:

1. `repowire/daemon/event_log.py`
   - Owns event buffering, event IDs/timestamps, dirty persistence, `events_since`, `subscribe`, and `unsubscribe`.
   - Registry and transports receive an `EventLog` dependency instead of using `PeerRegistry.add_event`.

2. `repowire/daemon/peer_access.py`
   - Pure helpers for resolving target/sender and enforcing circle boundaries using registry lookup methods.
   - Returns structured outcomes such as `(from_peer, target)` and preserves today's exact `ValueError` messages until routes have tests for HTTP mapping.

3. `repowire/daemon/peer_delivery.py`
   - Owns legacy delivery operations that are still on the registry today: `query`, WebSocket fallback `notify`, `deliver_ask`, and `broadcast`.
   - Depends on `PeerRegistry`, `MessageRouter`, `TransportRouter`, and `EventLog`.
   - Leaves `MessageRouter` as the raw WebSocket protocol sender and `TransportRouter` as the transport selector.

4. `repowire/daemon/repair.py`
   - Later extraction only. Owns lazy/active repair, ghost demotion, stale reaping, ask-stash redelivery, and pending-reply-loss emission.
   - This is higher risk because it coordinates registry state, transport disconnects, ask tracker mutation, and event emission.

## Smallest Safe First PRs

### PR 1: Extract EventLog With Compatibility Shims

Goal: move event buffer mechanics out of `PeerRegistry` while preserving the existing registry methods as delegating shims.

Likely touched:

- Add `repowire/daemon/event_log.py`
- Update `repowire/daemon/peer_registry.py`
- Update `repowire/daemon/app.py`
- Possibly update `repowire/daemon/deps.py` app state shape
- Add `tests/test_event_log.py`
- Keep existing `tests/test_sse_stream.py` and `tests/test_routes.py` passing unchanged

Compatibility rule: keep `peer_registry.add_event`, `get_events`, `events_since`, `subscribe_events`, and `unsubscribe_events` until route handlers are migrated in a later PR.

Tests to pin before/with extraction:

- `tests/test_sse_stream.py`
- `tests/test_routes.py::TestEvents`
- `tests/test_transport_router.py` event emission assertions
- A new unit test for persisted event load/save dirty behavior

Why first: event buffering is cohesive, mostly synchronous, and already behaves like a standalone service. The compatibility shims keep blast radius small.

### PR 2: Route-Level Access Resolver

Goal: introduce `peer_access.py` and migrate duplicated sender/target/circle resolution out of route handlers and delivery methods without changing delivery yet.

Likely touched:

- Add `repowire/daemon/peer_access.py`
- Update `routes/asks.py`
- Update `routes/messages.py`
- Optionally update `PeerRegistry.check_access` to delegate internally while keeping its public method
- Add focused tests in `tests/test_peer_access.py`

Public behavior to preserve:

- Unknown target remains 404 in `/notify` and `/ask`.
- Ambiguous display-name lookup remains 409 where currently mapped.
- Circle boundary remains 403 in ACP and WebSocket notify paths.
- Unresolved sender still logs/proceeds for notify/ask where current behavior permits it.
- Peer ID lookup remains unambiguous even when display names collide.

Tests to pin:

- `tests/test_circles.py`
- `tests/test_routes.py::TestMessages::test_notify_unknown_peer`
- `tests/test_routes.py::TestMessages::test_notify_ambiguous_peer_returns_409`
- `tests/test_ask_routes.py::TestOpenAsk`

### PR 3: Move Legacy Query/Notify/Broadcast Service Out of Registry

Goal: introduce `PeerDeliveryService` and migrate route and scheduler calls to it while leaving registry delivery methods as deprecated delegating wrappers for MCP/tests during the transition.

Implementation note: the `arch/delivery-service` slice adds
`repowire/daemon/peer_delivery.py`, wires `app.state.peer_delivery`, migrates
message routes and open-ask delivery to the service, aligns scheduler ask/notify
dispatch through it, and leaves registry delivery methods as WS-compatible
delegating shims. It deliberately does not change repair, lifecycle, stale
startup pruning, or the public `/notify` response contract.

Likely touched:

- Add `repowire/daemon/peer_delivery.py`
- Update `repowire/daemon/routes/messages.py`
- Update `repowire/daemon/routes/asks.py` for ack reply notify calls
- Update `repowire/daemon/scheduler.py`
- Update `repowire/daemon/deps.py` / app state construction
- Update tests that instantiate `PeerRegistry` directly with mocked routers

Public behavior to preserve:

- `/query` busy/offline preflight and timeout/error response text.
- Query event lifecycle: `query` pending, success/timeout/error update, response event truncation.
- `/notify` ACP-before-WebSocket ordering and 503 when no live connection.
- Notify return status: `sent` for online, `queued` for busy.
- `/broadcast` best-effort semantics and sender-circle filtering.
- Ask and ack reply framing, especially `[ack #cid from @peer]`.

Tests to pin:

- `tests/test_circles.py` for query/notify/broadcast access behavior
- `tests/test_routes.py::TestMessages`
- `tests/test_ask_routes.py`
- `tests/test_transport_router.py`
- `tests/test_message_router.py`
- Scheduler notify/ask tests in `tests/test_scheduler.py`

### PR 4: Extract Repair Coordinator

Goal: move lazy/active repair and ACP stashed reply redelivery into a coordinator after event and delivery services are separated.

Likely touched:

- Add `repowire/daemon/repair.py`
- Update `PeerRegistry.lazy_repair` and `active_repair` into delegating shims or move callers to the coordinator
- Update `app.py` startup/shutdown persistence calls
- Update lazy repair and ACP redelivery tests

Public behavior to preserve:

- Lazy repair remains demand-driven and debounced; no polling loops or eager disk writes.
- ACP peers with `metadata["acp"]` remain exempt from WebSocket-only demotion when the flag is enabled.
- Offline TTL reaping emits pointer-only `pending_reply_lost` before ask cleanup.
- Same-peer-id and identity-tuple stashed reply redelivery semantics remain unchanged.

Tests to pin:

- `tests/test_lazy_repair.py`
- ACP pending reply tests in `tests/test_ask_routes.py` and ACP broker tests
- `tests/test_description_ttl.py`
- `tests/test_orchestrator_liveness.py`

## Route Cleanup After Services Exist

Once services exist, route handlers should follow one consistent pattern:

- Parse and validate request models.
- Resolve dependencies from app state.
- Call one application service method.
- Map known domain exceptions to HTTP status codes.
- Return Pydantic response models.

Avoid moving protocol-specific details into routes. For example, WebSocket frame construction should stay in `MessageRouter` or `TransportRouter`, not in `routes/messages.py`.

## No-Risk Extraction Assessment

I did not implement an extraction in this pass. The smallest obvious candidate is event buffering, but even that changes construction and shutdown persistence behavior and touches SSE, dashboard history, transport-router event emission, and app lifespan. It is low risk after tests are pinned, but not a trivial no-risk edit.

`normalize_identity_path` could be mechanically moved to a tiny identity module, but that would not materially advance the registry/route split and would create churn without reducing coupling.

## Blockers and Open Questions

- The Beads issue lookup by title did not resolve locally, so this artifact uses the title from the assignment rather than a concrete issue ID.
- `PeerRegistry` is a public test fixture surface across many tests. The first extraction should preserve delegating methods to avoid a large synchronized test rewrite.
- App state currently exposes `peer_registry` as the single object routes need. Introducing `event_log` and `peer_delivery` should be done through app state in one PR, with compatibility fallback only if tests need it.
- `routes/messages.py` owns both message delivery and event streaming. Moving event streaming to a dedicated `routes/events.py` is reasonable after `EventLog` exists, but should not be part of PR 1.

## Recommendation

Start with PR 1 (`EventLog`) because it creates a clean dependency seam and removes non-registry state from `PeerRegistry` without changing routing behavior. Follow with the access resolver before moving delivery, because it lets `/ask`, `/notify`, legacy registry delivery, and future transport-neutral paths share one authorization rule set.

Do not begin with repair extraction. Repair has the most cross-module invariants and should wait until events and delivery are injectable services.
