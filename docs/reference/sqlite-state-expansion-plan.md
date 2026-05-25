# SQLite state expansion plan

Status: current storage architecture and migration notes for v0.14.x. Do not implement additional schema migration from this document without a follow-up implementation review.

## Current state

Repowire currently has one active daemon persistence style plus legacy import
helpers:

- SQLite state under `~/.repowire/state.db`.
- Legacy JSON files under `~/.repowire/`, kept as one-time import sources and
  downgrade artifacts, not active daemon write targets for migrated domains.

The SQLite path currently covers schedules, session bindings, runtime identity certificates, dashboard/session events, and peer session mappings. `StateDatabase` owns WAL mode, `synchronous=NORMAL`, foreign keys, busy timeout, schema versioning, and the `legacy_imports` audit table. `SQLiteScheduleStore` is adapter-compatible with `ScheduleStore`, imports `schedules.json` once when the SQL table is empty, and leaves the JSON file untouched for downgrade and backcompat. `SQLiteSessionBindingStore` persists binding identifiers, runtime source locators, cursors, provenance, resume capability metadata, lifecycle status, and daemon-minted runtime birth certificates. It deliberately does not persist raw transcript bodies. `SQLiteEventStore` imports legacy `events.json` once, appends/updates event payloads in SQLite, and seeds the bounded in-memory event window at daemon startup. `PeerRegistry` imports legacy `sessions.json` once into `peer_session_mappings` and stops writing new `sessions.json` state.

Other state remains JSON or in-memory:

- `sessions.json`: legacy peer/session mapping import source; retained untouched
  for downgrade/export compatibility.
- `events.json`: legacy dashboard event import source; retained untouched for
  downgrade/export compatibility.
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

Current SQLite slices:

- Schedules use the SQLite adapter.
- Peer mappings use SQLite; `sessions.json` is not written by the daemon app.
- Asks, query futures, transport state, and raw transcripts keep their current ownership.
- Session bindings are stored in SQLite as control/provenance metadata and are used by compatible timeline/transcript and session-control slices when an unambiguous binding exists.
- Runtime identity certificates are stored in SQLite as short-lived nonce-backed
  envelopes for MCP identity adoption and daemon-restart rehydration. They are
  peer identity proof, not pane kill/restart ownership proof.
- Dashboard/session events remain a bounded in-memory deque for route/SSE compatibility;
  persistence is backed by SQLite instead of new `events.json` writes.
- `events.json` remains in place for downgrade/export compatibility and one-time import.

## Event journal slice

Implemented in the default SQLite state path:

1. Add the event table in the daemon state migration.
2. Import legacy `events.json` once if the SQL event table is empty.
3. On `PeerRegistry.add_event()`, append the event to SQLite best-effort and still append to the in-memory deque.
4. Preserve current `/events`, `/events?since=...`, and `/events/stream` behavior initially by reading from the in-memory window.
5. On daemon startup with SQLite enabled, seed the in-memory event window from the newest SQL events.
6. Keep `events.json` untouched for downgrade/backcompat.

This slice intentionally does not change dashboard API semantics. It only adds a durable event journal and validates the shared SQLite lifecycle on a low-risk state domain.

## Deferred state

Keep peer identity mappings separate from session bindings.

Peer mappings affect peer ID reuse, display-name collision handling, circle restoration, role claims, description persistence, pane hijack protection, and clean takeover behavior. Their SQLite table is deliberately separate from session bindings: peer mappings record live peer identity state, while session bindings record durable workstream/runtime provenance for timeline and control surfaces.

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

Use the existing migration pattern:

- Treat `~/.repowire/state.db` as the active daemon store for migrated domains.
- Keep the legacy `experiments.sqlite_state` config key accepted but deprecated;
  it no longer selects active JSON persistence in the daemon app.
- Keep JSON files intact while import/downgrade/export compatibility exists.
- Import legacy JSON once only when the corresponding SQL table is empty.
- Record import success or failure in `legacy_imports`.
- Treat corrupt legacy JSON as a logged import error, not a daemon startup failure.
- Make migrations idempotent and update `PRAGMA user_version`.
- Keep downgrade possible by leaving JSON files in place while the opt-out path exists.

For the event journal specifically:

- Do not dual-delete or rename `events.json`.
- If SQLite append fails, log and keep the in-memory event path working.
- If SQLite import fails, start with an empty SQL event table and keep serving from the current in-memory behavior.
- Avoid eager background migration work. Import during store initialization only, then rely on normal event appends.

For peer mappings specifically:

- Do not write `sessions.json` from the daemon app.
- Leave any existing `sessions.json` file in place as the legacy import/export
  snapshot.
- Lower-level JSON adapters may remain for explicit compatibility tests, but
  app startup should not select them for migrated domains.

## Failure modes

Legacy JSON failure modes:

- `sessions.json` import failure is recorded in `legacy_imports` and mappings
  start from SQLite.
- `events.json` import failure is recorded in `legacy_imports` and the event
  deque starts from SQLite.
- `schedules.json` import failure is recorded in `legacy_imports` and schedules
  start from SQLite.
- `review_queue.json` corruption logs and starts empty.
- In-memory asks and pending replies vanish on daemon restart.

SQLite state failure policy:

- Database open or migration failure is a daemon startup failure for migrated
  state. Run `repowire doctor` and restart after fixing the state path.
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
- The legacy `experiments.sqlite_state` flag no longer changes daemon app
  storage ownership.

Implementation tests should avoid touching `.beads` or `uv.lock`.
