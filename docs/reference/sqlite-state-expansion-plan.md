# SQLite state expansion plan

Status: architecture recommendation for v0.13.x review, updated after the session-binding slices landed. Do not implement additional schema migration from this document without a follow-up implementation review.

## Current state

Repowire currently has two persistence styles:

- Experimental SQLite state under `~/.repowire/state.db`, gated by `experiments.sqlite_state`.
- JSON files under `~/.repowire/`, usually loaded into daemon memory and flushed by lazy repair or explicit mutation paths.

The SQLite path currently covers schedules and session bindings. `StateDatabase` owns WAL mode, `synchronous=NORMAL`, foreign keys, busy timeout, schema versioning, and the `legacy_imports` audit table. `SQLiteScheduleStore` is adapter-compatible with `ScheduleStore`, imports `schedules.json` once when the SQL table is empty, and leaves the JSON file untouched for downgrade and backcompat. `SQLiteSessionBindingStore` persists binding identifiers, runtime source locators, cursors, provenance, resume capability metadata, and lifecycle status. It deliberately does not persist raw transcript bodies.

Other state remains JSON or in-memory:

- `sessions.json`: persistent peer/session mappings inside `PeerRegistry`; still retained for peer identity reuse, daemon restart behavior, and downgrade/backcompat while the binding store hardens.
- `events.json`: dashboard event ring buffer persisted from `PeerRegistry`.
- `AskTracker`: in-memory ask/ack lifecycle with TTL eviction and in-memory pending ACP reply stashes.
- Agent transcripts: source-of-truth JSONL files owned by Claude Code or other agent runtimes, parsed on demand for history routes.
- `review_queue.json`: small low-frequency review queue state.

## Before and current

Before:

- Schedules can opt into SQLite.
- Dashboard events are a 500-item in-memory deque plus `events.json`.
- Session transcript history is reconstructed from runtime-owned transcript files.
- Peer mappings are restored from `sessions.json`.
- Open asks and pending replies are lost on daemon restart.

Current compatible SQLite slices:

- Schedules stay on the existing experimental SQLite adapter.
- Peer mappings, asks, query futures, transport state, and raw transcripts keep their current ownership.
- Session bindings are stored in SQLite as control/provenance metadata and are used by compatible timeline/transcript and session-control slices when an unambiguous binding exists.
- Dashboard/session events remain a 500-item in-memory deque plus `events.json`.
- `events.json` remains in place for downgrade/backcompat.

## Recommendation

Use SQLite next for the append-only dashboard/session event journal, not peer identity or asks.

This is the safest expansion because event persistence is mostly append-only, already normalized through `PeerRegistry.add_event()`, and directly supports the v0.13 session-native direction: persisted history plus realtime events in one timeline. It improves restart durability and historical query capability without changing routing correctness, peer identity reuse, ask reminder semantics, or transport behavior.

Recommended event table shape, subject to implementation review:

- `event_id TEXT PRIMARY KEY`
- `type TEXT NOT NULL`
- `timestamp TEXT NOT NULL`
- `peer_id TEXT`
- `peer_name TEXT`
- `session_id TEXT`
- `turn_id TEXT`
- `payload_json TEXT NOT NULL`

Indexes should support newest-first timeline reads and session-scoped dashboard queries:

- `(timestamp)`
- `(session_id, timestamp)`
- `(peer_id, timestamp)`
- `(type, timestamp)` if route usage needs it

Keep payload JSON as the compatibility envelope in the event-journal slice. Typed columns should be limited to query keys that are already stable.

## Next safe v0.13 slice

Implement a `SQLiteEventStore` behind `experiments.sqlite_state`:

1. Add the event table in the daemon state migration.
2. Import legacy `events.json` once if the SQL event table is empty.
3. On `PeerRegistry.add_event()`, append the event to SQLite best-effort and still append to the in-memory deque.
4. Preserve current `/events`, `/events?since=...`, and `/events/stream` behavior initially by reading from the in-memory window.
5. On daemon startup with SQLite enabled, seed the in-memory event window from the newest SQL events.
6. Keep `events.json` untouched for downgrade/backcompat during the experimental phase.

This slice intentionally does not change dashboard API semantics. It only adds a durable event journal and validates the shared SQLite lifecycle on a low-risk state domain.

## Deferred state

Keep peer identity mappings separate from session bindings for now.

Peer mappings affect peer ID reuse, display-name collision handling, circle restoration, role claims, description persistence, pane hijack protection, and clean takeover behavior. The landed binding table does not replace `PeerRegistry.SessionMapping` or `sessions.json`; it records durable workstream/runtime provenance for timeline and control surfaces. Moving peer identity mappings into SQLite should remain a separate review with an explicit downgrade/backcompat plan.

Defer asks and pending replies.

Ask state is currently process-local by design. Persisting asks could eventually fix restart loss for open asks and stashed ACP replies, but it changes visible lifecycle semantics:

- Open asks might reappear after daemon restart.
- Stashed replies need redelivery ordering guarantees.
- TTL eviction and `pending_reply_lost` events need exactly-one ownership.
- Quiesce barriers must remain atomic with ask registration.
- Peer rebind behavior must remain strict enough to avoid misdelivery.

Treat asks as a separate reliability design, not a table added during the generic state expansion.

Keep raw transcripts outside SQLite.

Runtime transcript JSONL files remain the raw source of truth. SQLite can later index normalized transcript turns for faster timeline queries, but it should not replace or mutate the agent-owned transcript files.

Keep review queue JSON for now.

The review queue is small, low-frequency, and already has atomic rewrite behavior. It does not need to move in the first expansion.

## Migration and backcompat

Use the existing schedule migration pattern:

- Gate new stores behind `experiments.sqlite_state`.
- Keep JSON files intact during the experimental phase.
- Import legacy JSON once only when the corresponding SQL table is empty.
- Record import success or failure in `legacy_imports`.
- Treat corrupt legacy JSON as a logged import error, not a daemon startup failure.
- Make migrations idempotent and update `PRAGMA user_version`.
- Keep downgrade possible by leaving JSON files in place until SQLite state is default-on and separately reviewed.

For the event journal specifically:

- Do not dual-delete or rename `events.json`.
- If SQLite append fails, log and keep the in-memory event path working.
- If SQLite import fails, start with an empty SQL event table and keep serving from the current in-memory behavior.
- Avoid eager background migration work. Import during store initialization only, then rely on normal event appends.

## Failure modes

Current JSON failure modes:

- `sessions.json` corruption is backed up and mappings start empty.
- `events.json` load failure logs a warning and the event deque starts empty.
- `schedules.json` corruption with the JSON store starts schedules empty and marks the store dirty.
- `review_queue.json` corruption logs and starts empty.
- In-memory asks and pending replies vanish on daemon restart.

SQLite event store failure policy:

- Database open or migration failure should disable the SQLite event store for that daemon lifetime and preserve current in-memory behavior.
- Per-event append failure should not break `/events/chat`, `/events/chat_delta`, SSE, or peer registry event emission.
- Legacy import failure should be visible in `legacy_imports` and logs, but should not block daemon startup.
- Startup seeding from SQLite should tolerate bad payload rows by skipping them and logging a warning.

## Tests

Required tests for the first slice:

- Migration is idempotent and preserves existing schedule behavior.
- Legacy `events.json` imports once when the SQL events table is empty.
- Corrupt `events.json` records an import error and daemon startup continues.
- `add_event()` appends to SQLite and still wakes SSE subscribers.
- Simulated SQLite append failure keeps the in-memory deque and route behavior working.
- Daemon restart with SQLite enabled seeds the in-memory event window from SQL newest-first data.
- `/events?since=...` keeps the current gap behavior.
- Event ordering is stable across same-timestamp events, using insertion order or an explicit sequence.
- No behavior changes when `experiments.sqlite_state` is false.

Implementation tests should avoid touching `.beads` or `uv.lock`.
