# SQLite State Train for v0.13

Status: architecture plan only. No implementation in this branch yet.

## Why this belongs in v0.13

Recent v0.13 work exposed the same failure mode across unrelated features: durable state is split between in-memory dictionaries and flat JSON files, while runtime behavior now depends on cross-cutting identity, ordering, and recovery guarantees.

Observed pressure points:

- ACP replies can be stashed in `AskTracker` but are process-local today.
- Peer takeover/reconnect logic depends on durable identity tuples, pane ownership, PID hints, and stale/offline reaping.
- Schedules store peer identifiers but not enough ownership metadata to survive peer renames/rebinding cleanly.
- Dashboard events are a bounded deque plus `events.json`; ordering and recovery are best-effort.
- Session timeline/history is split between live `chat_turn` events and replay from agent transcript JSONL.
- Stale/resumable peers need durable state distinct from live WebSocket connections.
- Orchestrator memory/search needs a queryable event/session spine, not another bespoke JSON file.

The train should land as v0.13-compatible internal storage work: no route behavior breakage, no config format migration, and no immediate promise that every transport/control path is fully transport-neutral.

## Principles

1. **Adapter first.** Route handlers and MCP tools keep their current behavior. Storage moves behind interfaces that can be backed by JSON/in-memory or SQLite.
2. **Dual-read, one-way migration.** On first SQLite boot, import JSON files if DB tables are empty. After import, SQLite is authoritative for migrated domains; JSON files are left in place as backups and compatibility artifacts unless an explicit legacy writer still needs them.
3. **Runtime is not durable state.** WebSocket objects, asyncio tasks, SSE subscribers, relay connections, and ACP client handles stay process-local.
4. **Config remains YAML.** User-authored config stays in `~/.repowire/config.yaml`; SQLite stores daemon-owned state.
5. **Markdown remains human-authored.** Project docs, memory, issue text, and other intentionally editable markdown do not move to SQLite.
6. **Lazy repair survives.** Do not introduce polling loops just because a DB exists. Retain request-triggered cleanup and explicit scheduler wakeups.
7. **Small, recoverable slices.** Each PR must have a fallback path and migration tests before the next domain moves.

## Current storage/state surfaces reviewed

- `repowire/daemon/ask_tracker.py`
  - In-memory ask lifecycle, quiesce barriers, pending ACP reply stash, TTL eviction.
- `repowire/daemon/peer_registry.py`
  - In-memory peers, JSON-backed `sessions.json` mappings, bounded event deque backed by `events.json`, lazy repair/reaping.
- `repowire/daemon/schedule_store.py` and `repowire/daemon/scheduler.py`
  - JSON-backed schedules, in-process scheduler task, explicit wake event.
- `repowire/daemon/routes/messages.py`
  - Chat event ingestion, streaming deltas, bounded finalized-turn set, `/events` and SSE stream.
- `repowire/daemon/routes/peers.py` and `repowire/session/history.py`
  - Transcript route replays Claude Code transcript JSONL by peer path; Codex history discovery is deferred.
- `repowire/config/models.py`
  - YAML config, auth tokens, relay settings, experiment flags.
- `repowire/daemon/review_queue_store.py`
  - JSON-backed review queue.
- Existing local file names expected under `~/.repowire`: `config.yaml`, `sessions.json`, `events.json`, `schedules.json`, `review_queue.json` when used.

## What moves to SQLite

### Move in v0.13 train

1. **Peer identity and durable peer records**
   - Current source: `sessions.json` plus in-memory `Peer` objects.
   - Move: session/peer mappings, stable display name identity, circle, backend, path, role, description, agent PID, machine when known, last-known pane/tmux metadata where useful for diagnostics.
   - Keep process-local: live status derived from active transport connection, actual WebSocket connection, SSE subscribers.

2. **Ask lifecycle**
   - Current source: in-memory `AskTracker`.
   - Move: open/closed asks, endpoints, text, timestamps, close reason, `reply_to`, pending ACP reply, pending-reply identity tuple, quiesce markers.
   - Benefit: daemon restart no longer drops open asks or stashed ACP replies.

3. **Schedules**
   - Current source: `schedules.json`.
   - Move: one-shot and recurring schedules, owner metadata, delivery target, cron, next fire time, last fire outcome.
   - Keep process-local: scheduler task and wake event.

4. **Dashboard/events spine**
   - Current source: bounded deque plus `events.json`.
   - Move: append-only event log with monotonic sequence and event UUID.
   - Preserve route behavior: `/events` still returns the recent window; `since=<event_id>` still works. Internally, use DB sequence to avoid ordering ambiguity.

5. **Chat turns/timeline index**
   - Current source: live `chat_turn`/`chat_turn_delta` events plus on-demand transcript replay.
   - Move: normalized table for ingested chat turns and deltas, linked to peer/session/turn IDs, while still emitting events for dashboard compatibility.
   - Keep transcript files as source material: Claude/Codex/Gemini native JSONL transcripts remain external artifacts. SQLite stores Repowire's index/cache, not the only copy of raw transcripts.

6. **Review queue**
   - Current source: `review_queue.json`.
   - Move after core state because it is low-risk and benefits from shared migrations/backup mechanics.

7. **Future orchestrator memory/search index**
   - Add tables only after event/timeline tables exist. Prefer FTS5 over ad hoc JSON search if bundled SQLite supports it; otherwise ship a plain indexed fallback.

### Stay YAML / markdown / files

- `~/.repowire/config.yaml`: daemon host/port, auth token, relay API key, bot tokens, spawn allowlists, experiment flags.
- Agent runtime configs managed by installers: `~/.claude/settings.json`, `~/.claude.json`, `~/.codex/*`, `~/.gemini/settings.json`, `~/.opencode/*`.
- Human-authored docs and repo memory markdown.
- Attachments under `~/.repowire/attachments/`: binary/blob files stay in the filesystem; SQLite may store metadata later if needed.
- Native agent transcript JSONL files under agent-owned directories. SQLite may index turns, but should not rewrite or delete these files.
- Cache/log/lock/PID files under cache directories.

## High-level module layout

Proposed package:

```text
repowire/daemon/state/
  __init__.py
  database.py          # connection lifecycle, pragmas, transactions
  migrations.py        # schema versioning and JSON import helpers
  models.py            # small dataclasses/row mappers, not Pydantic API models
  peers.py             # PeerStore protocol + SQLitePeerStore
  asks.py              # AskStore protocol + SQLiteAskStore
  schedules.py         # ScheduleStore protocol + SQLiteScheduleStore
  events.py            # EventStore protocol + SQLiteEventStore
  timeline.py          # ChatTimelineStore / transcript index
  reviews.py           # ReviewQueueStore-compatible adapter
  maintenance.py       # backup, integrity_check, vacuum policy, pruning helpers
```

Keep existing public classes initially, but let them accept store objects:

- `PeerRegistry(..., peer_store=...)`
- `AskTracker(..., ask_store=...)`
- `Scheduler(store=...)` where `ScheduleStore` becomes an interface-compatible adapter.
- `ReviewQueueStore` keeps method names while delegating to SQLite in the migrated implementation.

## Database location and pragmas

Default DB path: `~/.repowire/state.db`.

Startup should create the directory with user-only permissions where possible. Recommended pragmas:

- `PRAGMA journal_mode=WAL;`
- `PRAGMA synchronous=NORMAL;`
- `PRAGMA foreign_keys=ON;`
- `PRAGMA busy_timeout=5000;`
- `PRAGMA user_version=<schema_version>;`

Use one daemon-owned writer connection plus short-lived/read-only connections only if needed. The current daemon is a single process; optimize for correctness and simple async integration, not multi-process concurrency.

## High-level schema

Names are provisional; final implementation should evolve through migrations.

### `schema_migrations`

- `version INTEGER PRIMARY KEY`
- `applied_at TEXT NOT NULL`
- `description TEXT NOT NULL`

Alternatively use `PRAGMA user_version` plus this table for auditability.

### `peers`

Durable peer/session identity.

- `peer_id TEXT PRIMARY KEY`
- `display_name TEXT NOT NULL`
- `circle TEXT NOT NULL`
- `backend TEXT NOT NULL`
- `path TEXT`
- `normalized_path TEXT`
- `machine TEXT`
- `role TEXT NOT NULL`
- `description TEXT NOT NULL DEFAULT ''`
- `description_set_at TEXT`
- `agent_pid INTEGER`
- `last_seen TEXT`
- `last_status TEXT` for diagnostics/restart presentation only; live status still reconciles with transport.
- `last_turn_state TEXT`
- `pane_id TEXT`
- `tmux_session TEXT`
- `metadata_json TEXT NOT NULL DEFAULT '{}'`
- `created_at TEXT NOT NULL`
- `updated_at TEXT NOT NULL`
- `reaped_at TEXT`

Indexes:

- `(display_name, circle, backend)` for mapping reuse.
- `(circle, display_name)` for peer lookup.
- `(backend, normalized_path)` for identity adoption/rebind.
- `(pane_id)` where not null for pane lookup/release.

### `peer_identity_claims` (optional but useful)

Tracks takeover/reconnect history without bloating `peers`.

- `id INTEGER PRIMARY KEY AUTOINCREMENT`
- `peer_id TEXT NOT NULL REFERENCES peers(peer_id)`
- `display_name TEXT NOT NULL`
- `circle TEXT NOT NULL`
- `backend TEXT NOT NULL`
- `normalized_path TEXT`
- `machine TEXT`
- `pane_id TEXT`
- `agent_pid INTEGER`
- `parent_pid INTEGER`
- `claim_type TEXT NOT NULL` (`new`, `reconnect`, `takeover`, `rejected_pane_hijack`, `reaped`)
- `created_at TEXT NOT NULL`

### `asks`

Durable ask/ack lifecycle.

- `correlation_id TEXT PRIMARY KEY`
- `from_peer_id TEXT NOT NULL`
- `from_peer_name TEXT NOT NULL`
- `to_peer_id TEXT NOT NULL`
- `to_peer_name TEXT NOT NULL`
- `text TEXT NOT NULL`
- `reply_to TEXT`
- `created_at TEXT NOT NULL`
- `closed_at TEXT`
- `closed INTEGER NOT NULL DEFAULT 0`
- `close_reason TEXT`
- `pending_reply TEXT`
- `pending_reply_at TEXT`
- `asker_identity_json TEXT`
- `updated_at TEXT NOT NULL`

Indexes:

- `(to_peer_id, closed, created_at DESC)` for pending inbound.
- `(from_peer_id, closed, created_at DESC)` for pending outbound and redelivery.
- `(created_at)` for TTL eviction.
- `(reply_to)` for thread chaining diagnostics.

### `ask_quiesce`

- `peer_id TEXT PRIMARY KEY`
- `created_at TEXT NOT NULL`
- `reason TEXT`

Keeps switch barriers durable enough to recover explicitly after crash. On daemon boot, stale quiesce rows older than a short TTL can be cleared during lazy repair or marked abandoned.

### `schedules`

- `schedule_id TEXT PRIMARY KEY`
- `from_peer TEXT NOT NULL`
- `from_peer_id TEXT`
- `to_peer TEXT NOT NULL`
- `to_peer_id TEXT`
- `text TEXT NOT NULL`
- `kind TEXT NOT NULL` (`ask`, `notify`)
- `circle TEXT`
- `fire_at TEXT NOT NULL`
- `cron TEXT`
- `created_at TEXT NOT NULL`
- `updated_at TEXT NOT NULL`
- `last_fired_at TEXT`
- `last_outcome TEXT`
- `last_error TEXT`

Index `(fire_at)` for `next_due()`.

### `events`

Append-only dashboard/system event log.

- `seq INTEGER PRIMARY KEY AUTOINCREMENT`
- `event_id TEXT NOT NULL UNIQUE`
- `type TEXT NOT NULL`
- `timestamp TEXT NOT NULL`
- `peer_id TEXT`
- `peer_name TEXT`
- `session_id TEXT`
- `turn_id TEXT`
- `payload_json TEXT NOT NULL`

Indexes:

- `(event_id)` for `since` compatibility.
- `(timestamp, seq)` for timeline queries.
- `(peer_id, timestamp)` and `(session_id, timestamp)` for dashboard/history.
- Optional partial indexes for `type='chat_turn'` and `type='chat_turn_delta'` only if query plans need them.

### `chat_turns`

Normalized durable timeline view of final chat turns.

- `id INTEGER PRIMARY KEY AUTOINCREMENT`
- `event_seq INTEGER REFERENCES events(seq)`
- `peer_id TEXT`
- `peer_name TEXT NOT NULL`
- `role TEXT NOT NULL`
- `text TEXT NOT NULL`
- `session_id TEXT`
- `turn_id TEXT`
- `timestamp TEXT NOT NULL`
- `tool_calls_json TEXT NOT NULL DEFAULT '[]'`
- `source TEXT NOT NULL` (`hook`, `transcript_import`, `acp`, etc.)

Uniqueness should be conservative to avoid dropping valid turns:

- Unique `(peer_id, session_id, turn_id, role)` where `turn_id IS NOT NULL` after late-delta/final reconciliation is understood.

### `chat_turn_deltas`

- `id INTEGER PRIMARY KEY AUTOINCREMENT`
- `event_seq INTEGER REFERENCES events(seq)`
- `peer_id TEXT`
- `peer_name TEXT NOT NULL`
- `session_id TEXT`
- `turn_id TEXT NOT NULL`
- `chunk_index INTEGER NOT NULL`
- `kind TEXT NOT NULL`
- `text TEXT NOT NULL`
- `tool_call_json TEXT`
- `is_final INTEGER NOT NULL DEFAULT 0`
- `timestamp TEXT NOT NULL`

Unique `(peer_id, session_id, turn_id, chunk_index)` where practical. The existing finalized-turn drop behavior must remain route-compatible.

### `review_queue`

- `reviewer TEXT NOT NULL`
- `pr_url TEXT NOT NULL`
- `last_reviewed_sha TEXT`
- `recorded_at TEXT NOT NULL`
- PRIMARY KEY `(reviewer, pr_url)`

### `attachments` metadata (defer unless needed)

Keep blobs/files on disk. If added:

- `attachment_id TEXT PRIMARY KEY`
- `filename TEXT`
- `content_type TEXT`
- `size_bytes INTEGER`
- `path TEXT NOT NULL`
- `created_at TEXT NOT NULL`
- `expires_at TEXT`

## Migration and backcompat story

### Input files

- `~/.repowire/sessions.json` -> `peers`
- `~/.repowire/events.json` -> `events` and chat timeline tables where event type matches
- `~/.repowire/schedules.json` -> `schedules`
- `~/.repowire/review_queue.json` -> `review_queue`
- No source file for `asks`; only future asks become durable unless a short transitional snapshot is added before the switch.

### Import rules

1. On startup, open SQLite and run migrations.
2. For each migrated domain, if the target table is empty and the legacy JSON file exists, import it inside a transaction.
3. Record import metadata in a `legacy_imports` table:
   - `source_path`, `source_mtime`, `source_size`, `imported_at`, `row_count`, `status`, `error`.
4. Leave source JSON untouched. If corrupt, rename/backup behavior should mirror current code where it already backs up corrupt mappings; otherwise log and continue with empty table for that domain.
5. Once a domain is SQLite-authoritative, stop writing its JSON file except for an optional one-release debug/export flag. Do not dual-write indefinitely.
6. Keep route response models and endpoint semantics unchanged.

### Compatibility switches

Add a narrow internal storage flag, preferably hidden/experimental during rollout:

```yaml
experiments:
  sqlite_state: true
```

Default can become on within v0.13 after migration tests and recovery tooling land. Before default-on, adapter classes should support both paths so bugs can be isolated without reverting large route changes.

### Downgrade story

SQLite migration is one-way for authoritative state. Downgrade should be recoverable by:

- Keeping original JSON files as `.pre-sqlite` or untouched imports.
- Providing `repowire state export-json` later if downgrade/export is required.
- Documenting that new state created after SQLite enablement may not appear to old versions unless exported.

## Transaction and locking model

### SQLite transaction boundaries

Use explicit transactions around multi-step invariants that currently rely on Python locks:

- Ask register: insert ask only if neither endpoint is quiesced.
- Ask ack-with-message: read ask, attempt delivery outside DB transaction if needed, then close in a short transaction after delivery succeeds.
- ACP stash: update pending reply and identity tuple atomically.
- Pending reply redelivery: select candidates, deliver outside transaction, then `UPDATE ... WHERE closed=0 AND pending_reply IS NOT NULL` to close/clear atomically.
- Peer allocate/register: resolve/adopt mapping, release pane from prior owner, upsert peer row, append identity claim, all in one transaction.
- Schedule fire: claim due schedule before delivery to avoid double-fire after wake/race; then delete or reschedule with outcome.
- Event ingest: insert `events` row and corresponding `chat_turn(s)` row in one transaction.

### Python locks remain useful

SQLite does not replace all in-process coordination:

- `PeerRegistry._lock` remains around in-memory peer snapshots and transport-adjacent decisions.
- `AskTracker._lock` can be reduced but should remain while callers assume coherent snapshots.
- Scheduler still uses `_wake` and a task; the DB only backs `next_due()` and schedule mutation.
- SSE subscriber set remains in memory.

### Avoid long DB locks

Do not hold SQLite write transactions across network/transport delivery. Pattern:

1. Read/mark candidate state in DB if necessary.
2. Release transaction.
3. Perform WebSocket/ACP/notify delivery.
4. Re-open short transaction to finalize state with compare-and-set predicates.

### Busy handling

Set `busy_timeout`. Treat persistent `database is locked` as a 503/temporary internal failure only where safe. Log with domain and operation. Keep writes small and indexed.

## PR slices, risk gates, and tests

### PR 1: State DB foundation, no domain migration

Deliver:

- `repowire/daemon/state/database.py` and `migrations.py`.
- Schema version table/user_version.
- Startup/shutdown integration behind `experiments.sqlite_state` or internal constructor injection.
- Integrity check command or internal helper.

Risk gate:

- No existing route behavior changes.
- DB can be created under temp HOME and closed cleanly.

Tests:

- Migration idempotence.
- WAL/pragmas applied.
- Corrupt/unopenable DB error path is explicit and recoverable.

### PR 2: Peer mappings adapter (`sessions.json` -> SQLite)

Deliver:

- `PeerStore` protocol.
- JSON-backed implementation preserving current behavior.
- SQLite implementation behind flag.
- Import from `sessions.json`.
- `PeerRegistry` uses store for mappings while keeping live `_peers` behavior.

Risk gate:

- Peer IDs/display names remain stable across daemon restart.
- Clean takeover, circle adoption, pane hijack guard, display-name ambiguity behavior unchanged.

Tests:

- Import valid `sessions.json`.
- Corrupt JSON backup/log behavior.
- Reconnect with same peer ID.
- Offline name reclaim.
- Cross-circle adoption.
- Pane release and pane-hijack rejection.

### PR 3: Ask tracker durability

Deliver:

- `AskStore` protocol and SQLite implementation.
- Durable open asks, close reasons, pending ACP replies, identity tuple, quiesce rows.
- TTL eviction via existing lazy paths.

Risk gate:

- `/ask`, `/ack`, `/asks/pending` response behavior unchanged.
- No reply is marked closed before successful delivery unless current route already does so.

Tests:

- Open ask survives tracker/app restart and appears in pending.
- Bare ack idempotence.
- Ack-with-message 503 leaves ask open.
- ACP pending reply survives restart and redelivers/clears on reconnect.
- Identity-tuple rebind only when unambiguous.
- Quiesce rejects new asks and clears safely after failure/restart policy.
- TTL eviction preserves current stashed-reply loss-event ordering.

### PR 4: Schedule store SQLite

Deliver:

- SQLite schedule adapter with same public methods.
- Import from `schedules.json`.
- Optional schedule ownership enrichment (`from_peer_id`, `to_peer_id`) without route response changes.
- Atomic claim/reschedule/delete semantics.

Risk gate:

- Existing scheduler wake/no-poll model unchanged.
- Past-due schedules still fire immediately on startup.

Tests:

- Create/list/delete parity.
- One-shot fire removes schedule.
- Cron fire advances next time.
- Delivery failure behavior unchanged.
- Restart before/after due time.

### PR 5: Event log SQLite, `/events` compatibility

Deliver:

- SQLite event store.
- `PeerRegistry.add_event`, `get_events`, `events_since`, `_update_event` backed by store.
- In-memory subscriber wakeups retained.
- Import last window from `events.json`.

Risk gate:

- Dashboard still receives same JSON event shapes.
- `/events?since=<id>` fallback behavior remains compatible when ID is absent.
- SSE stream remains event-driven.

Tests:

- Event order is insertion-stable for same timestamp.
- Recent-window limit is preserved for route output.
- `since` returns events after ID.
- Missing/evicted ID returns current window.
- SSE initial flush and subsequent event delivery.

### PR 6: Chat timeline index

Deliver:

- Insert normalized `chat_turns`/`chat_turn_deltas` alongside events.
- Keep final chat_turn as authoritative.
- Start adding read helpers for future session timeline route without changing current transcript route yet.

Risk gate:

- Existing dashboard rendering unchanged.
- Late delta drop behavior unchanged.

Tests:

- Final turn marks finalized and drops late deltas.
- Deltas and final turn reconcile by `(peer_id, session_id, turn_id)`.
- Tool calls preserved.
- Restart preserves timeline rows.

### PR 7: Transcript/session history bridge

Deliver:

- Optional transcript indexer that imports native transcript turns into `chat_turns` with `source='transcript_import'`.
- Keep `GET /peers/{name}/transcript` route behavior unchanged initially; then internally read from SQLite only after parity is proven.
- Track transcript file path, mtime, line offsets in a separate index table if incremental indexing is added.

Risk gate:

- Claude Code transcript route returns same turns/cursors as before.
- Codex remains no worse than current deferred behavior unless explicitly implemented.

Tests:

- Cursor compatibility.
- Same-timestamp stable ordering.
- Tool-only/tool-result-only filtering parity.
- Corrupt JSONL lines ignored.

### PR 8: Review queue and maintenance tooling

Deliver:

- SQLite-backed review queue adapter.
- Import `review_queue.json`.
- `repowire state doctor`, `repowire state backup`, possibly `repowire state export-json`.

Risk gate:

- MCP review tools/routes keep response shape.

Tests:

- Upsert/list/delete parity.
- Import malformed entries defensively.
- Backup file creation and integrity check.

### PR 9: Default-on and cleanup

Deliver:

- Enable SQLite state by default after previous slices have passed on real use.
- Keep legacy JSON import paths for at least one minor/patch window.
- Remove indefinite dual-write if any existed.
- Update public docs.

Risk gate:

- Fresh install, upgrade from JSON state, and rollback/export story are documented.
- CI covers route parity under SQLite.

## Rollout and recovery plan

1. **Hidden opt-in:** land DB foundation with no behavior changes.
2. **Domain opt-in:** migrate one domain at a time behind constructor/experiment flags.
3. **Shadow read where useful:** for peer mappings and schedules, compare JSON import count and SQLite count in logs during development, but avoid permanent dual-write.
4. **Default-on within v0.13:** after ask, peer, schedule, and event stores are stable.
5. **Backups:** before first import from each JSON file, copy it to a timestamped `.pre-sqlite` backup or leave untouched and record import metadata. Before destructive schema migrations, copy `state.db` to `state.db.backup-<timestamp>` unless size makes that unreasonable.
6. **Recovery commands:** add a CLI surface before default-on:
   - `repowire state doctor` -> open DB, run `PRAGMA integrity_check`, show schema version and row counts.
   - `repowire state backup` -> checkpoint WAL and copy DB/WAL/SHM safely.
   - `repowire state export-json` -> optional downgrade/debug helper.
7. **Corruption behavior:** if DB open or integrity check fails, do not silently start with empty state. Rename the bad DB only under explicit recovery command or after creating a backup and logging loudly. For daemon startup, prefer fail-fast with actionable instructions over data loss.
8. **WAL checkpoint:** on clean daemon shutdown, checkpoint WAL opportunistically. Do not make shutdown depend on successful vacuum/checkpoint.
9. **Pruning:** preserve existing lazy repair semantics. Use DB deletes for TTL/prune, triggered by current endpoints/startup/shutdown paths, not a polling loop.

## Security and privacy concerns

- The DB will contain peer paths, machine names, chat turns, ask text, schedule text, pending replies, review URLs, and event payloads. Treat it as sensitive local user data.
- Store `state.db`, `state.db-wal`, and `state.db-shm` under `~/.repowire` with user-only permissions where possible.
- Do not move auth tokens/API keys from YAML into SQLite. Existing config remains the secret-bearing file.
- Avoid logging message bodies, pending replies, schedule text, and full event payloads during migration errors. Log counts and IDs instead.
- Backups/export files inherit the same sensitivity as the DB; create them with restrictive permissions and clear naming.
- Relay/dashboard surfaces must not expose new raw DB endpoints. State tooling should remain local CLI or authenticated localhost-only routes if routes are needed.
- If FTS is added, remember it duplicates text in index tables; purge/delete paths must clean both content and FTS rows.
- Do not store binary attachments in SQLite unless there is a specific product reason. File paths plus metadata are enough.

## Open blockers / decisions needed

1. **SQLite dependency strategy.** Python stdlib `sqlite3` is enough for sync operations, but async route handlers need a deliberate wrapper (`asyncio.to_thread`, single writer executor, or an async library). Avoid adding a dependency unless tests show the stdlib wrapper is awkward.
2. **DB default-on timing.** Need Prass decision on whether `experiments.sqlite_state` ships default-off for one patch or becomes default-on as soon as core domains pass.
3. **Ask restart semantics.** Durable open asks are valuable, but after daemon restart the recipient transport may not have seen the original injected ask. Stop-hook reminders cover this for hooks, but non-hook transports need explicit parity review.
4. **Quiesce crash policy.** A persisted quiesce barrier can prevent orphaning asks, but if the daemon crashes mid-switch it can also block forever. Need a TTL/owner policy.
5. **Event retention.** Current dashboard keeps 500 events. SQLite enables more. Need retention policy for DB growth: keep all for now, prune by age/count, or configurable.
6. **Timeline source of truth.** Decide when SQLite timeline becomes authoritative versus a cache over native transcripts. For v0.13, safest framing is index/cache plus current route compatibility.
7. **Migration docs.** Default-on requires README/reference docs updates and a recovery note. Planning doc alone is not enough for the shipping PR.

## Summary recommendation

Land SQLite as a v0.13-compatible state spine, starting with infrastructure and peer mappings, then asks, schedules, events, and timeline indexing. Keep YAML config and human-authored markdown/file artifacts where they are. The highest-value early win is durable asks plus peer identity because that directly addresses ACP stashed replies and takeover/rebind bugs; the highest-risk slice is event/timeline because dashboard behavior depends on ordering and streaming compatibility.
