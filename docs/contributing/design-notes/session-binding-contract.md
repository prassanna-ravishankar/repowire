# Session binding contract

Status: architecture contract for the v0.13 session-native train, updated after
the compatible SQLite binding-store, binding lookup, and session-control slices
landed. This document defines the boundary current and future implementation
slices should preserve; it does not claim dashboard controls, approval UI,
backend-specific resume execution, or full transport-neutral routing are
shipped.

## Problem

Repowire currently has several identity concepts that are useful but not the
same thing:

- `Peer.peer_id` is the daemon-assigned live routing identity for the current
  executor.
- `PeerRegistry.SessionMapping` persists peer reuse metadata in
  `sessions.json`.
- Hook payload `session_id` values identify a backend runtime transcript
  session.
- Transcript and rollout files are backend-owned source history.
- Timeline routes merge runtime history with daemon events by peer, path,
  runtime `session_id`, and turn IDs.
- Ask, approval, delivery, and resume work still mostly target live peers or
  transport-specific state.

A durable Repowire session binding makes those relationships explicit without
turning Repowire into the owner of raw runtime transcript bodies.

## Boundary

Raw runtime history remains canonical in the source data owned by the backend
runtime. Repowire may persist:

- bindings between a Repowire session and backend runtime sessions;
- provenance pointers and cursors into source history;
- current executor/control state;
- resumability metadata;
- delivery, approval, and command state;
- rebuildable indexes derived from source history.

Repowire must not persist raw transcript bodies as the authoritative record.
If a future timeline/search index stores normalized excerpts or summaries for
query speed, those rows are cache/index material. They must be rebuildable from
source history and must carry provenance back to the runtime source locator.

## Binding model

A session binding joins four identities:

- **Repowire session:** the durable workstream/control identity users and
  dashboards target.
- **Backend runtime session:** the runtime's own session identifier, such as a
  Claude Code transcript session ID or Codex rollout session ID.
- **Current peer/runtime executor:** the live or last-known executor that can
  receive mesh messages now.
- **Source history locator:** the file, database, or runtime-native handle where
  raw history can be replayed.

Recommended persisted fields:

| Field | Purpose |
| --- | --- |
| `repowire_session_id` | Stable Repowire workstream ID. This is the primary target for future session commands. |
| `peer_id` | Current or last-known executor peer ID. Nullable when no executor is attached. |
| `current_executor_peer_id` | Alias-compatible name for command surfaces that need to distinguish session owner from active executor. |
| `backend` | Runtime backend, using the existing `AgentType` values. |
| `project_path` | Runtime working directory used for identity matching and history discovery. Store the daemon-local canonical path; do not expose it in public memory. |
| `runtime_session_id` | Backend-native session/transcript ID from hooks or source metadata. |
| `runtime_source_uri` | Opaque source locator, for example `claude-jsonl:<encoded-project>/<uuid>.jsonl` or `codex-rollout:<date>/<filename>.jsonl`. |
| `source_cursor` | Last indexed source position, such as line offset, byte offset, sequence, or backend cursor. |
| `provenance` | Structured pointer set for rebuilding and auditing derived state. See below. |
| `resume_capability` | Backend-specific resume hints and support level. |
| `created_at` | When the Repowire binding was created. |
| `last_seen_at` | Last time hooks, transport, or source replay confirmed the binding. |
| `status` | Binding lifecycle status. |
| `metadata` | Small compatibility envelope for backend-specific hints. Keep raw text out. |

`peer_id` and `current_executor_peer_id` can start as the same value. Keep both
names in the contract because later slices may attach a new executor to an
existing session while retaining the original peer provenance.

## Statuses

Use binding status for durable session lifecycle, not live socket state:

- `active`: a live executor is attached and should be commandable.
- `detached`: the runtime session exists, but no live executor is attached.
- `resumable`: the binding has enough backend metadata to attempt resume.
- `archived`: the workstream is intentionally kept for history only.
- `lost`: the binding points at source history that can no longer be found.
- `superseded`: a newer binding replaced this one during migration, merge, or
  conflict repair.

`PeerStatus` (`online`, `busy`, `offline`) and `TurnState` remain live executor
state. Do not overload them as durable session lifecycle.

## Provenance

Every derived session/timeline/control row should be traceable to one or more
source pointers. A provenance object should be structured, not a free-form
string:

```json
{
  "source_kind": "runtime_transcript",
  "backend": "claude-code",
  "runtime_session_id": "runtime-owned-id",
  "runtime_source_uri": "claude-jsonl:project/session.jsonl",
  "source_cursor": {"line_offset": 120},
  "source_event_id": "backend-message-or-turn-id",
  "observed_by_peer_id": "repow-dev-a1b2c3d4",
  "observed_at": "2026-05-21T12:00:00Z"
}
```

Rules:

- Source locators identify where to replay history; they are not transcript
  bodies.
- Cursors are monotonic within a source locator when the backend exposes an
  ordered source.
- Derived daemon events should carry enough provenance to be de-duplicated
  against a later replay of source history.
- If a backend cannot expose source history, record `source_kind:
  "runtime_unavailable"` and keep the binding useful for control state only.
- Failed source lookups should move the binding toward `lost` only after lazy
  repair has verified the source is still missing. Do not add polling.

## Existing touchpoints

- `repowire/protocol/peers.py` defines live peer identity, backend, path,
  machine, metadata, status, and turn state.
- `repowire/daemon/peer_registry.py` persists `SessionMapping` in
  `StateDatabase` and imports legacy `sessions.json` once. It owns peer ID reuse, display-name
  collision handling, role claims, descriptions, pane hijack evidence, and
  ghost eviction.
- `daemon-go/hooks/session.go` registers peers on `SessionStart` with
  backend, path, pane, role, turn state, agent PID, and metadata such as
  `hook_session_id`.
- `daemon-go/hooks/stop.go` posts chat turns with runtime `session_id`,
  turn IDs, pane IDs, and transcript-derived tool summaries.
- `repowire/session/history.py` discovers and replays backend-owned history for
  Claude Code and Codex by project path and runtime metadata.
- `repowire/daemon/routes/peers.py` exposes `/peers/{name}/timeline` and
  `/peers/{name}/transcript`, resolving through an unambiguous session binding
  when available and otherwise preserving peer/path/backend compatibility plus
  optional runtime `session_id` filtering.
- `repowire/daemon/routes/messages.py` ingests live `chat_turn` and
  `chat_turn_delta` events using runtime `session_id` and `turn_id` for
  dashboard reconciliation.
- `repowire/daemon/ask_tracker.py` keeps open asks and pending replies
  in-memory, with peer IDs as routing endpoints and strict identity snapshots
  for ACP reply rebind.
- `repowire/daemon/peer_delivery.py` coordinates ask/notify delivery and
  already exposes explicit delivered/queued outcomes for some paths.
- `repowire/daemon/state/database.py` is the experimental daemon SQLite wrapper
  currently used by schedules and session bindings. The existing
  `rfcs/sqlite-state-expansion-plan.md` recommends expanding SQLite
  carefully and keeping raw transcripts outside SQLite.
- `repowire/daemon/state/session_bindings.py` persists binding metadata,
  source locators, cursors, provenance, resume capability, and lifecycle
  status without storing raw runtime transcript bodies.
- `repowire/daemon/session_control.py` is the first session-acquisition service
  used by durable jobs. It records a `session.acquire_executor` operation,
  then resolves a live peer, backend-native resume, or fresh spawn.
- `repowire/daemon/state/operations.py` stores internal operation lifecycle
  records for acquisition attempts, strategies, results, and structured
  failures. Operations are audit/control records; they do not replace peer,
  session, or job identity.

## Migration path

1. **Observe, do not rewire.** Start by writing bindings from existing
   SessionStart, Stop, history replay, and timeline inputs. Keep current peer
   routes and hook behavior unchanged.
2. **Backfill from PeerRegistry metadata.** For each `SessionMapping`, create a
   detached binding with `peer_id`, `backend`, `project_path`, `created_at` from
   first observation if available, and `last_seen_at` from mapping update time.
   Leave `runtime_session_id` and source locator empty until hooks or history
   replay prove them.
3. **Attach runtime sessions from hooks.** Stop and SessionStart hooks should
   update binding observations with backend runtime `session_id`,
   transcript/source path, turn IDs, and source cursors. The hook should not
   upload raw transcript text as binding state.
4. **Let timeline/transcript routes resolve through bindings.** Add
   session-targeted routes after bindings exist. Existing
   `/peers/{name}/timeline` and `/peers/{name}/transcript` remain compatibility
   routes and can internally resolve to a binding when the peer/path/session
   tuple is unambiguous.
5. **Move controls to session targets.** Resume, send-message, schedule,
   approvals, and future model/backend controls should target
   `repowire_session_id`, then resolve to an active executor or a resume plan.
   Peer-targeted controls stay as compatibility shims.
6. **Keep ask and delivery compatibility.** Asks, acks, approvals, and delivery
   receipts should initially store peer endpoints plus optional
   `repowire_session_id` pointers. Do not change reminder, TTL, redelivery, or
   quiesce semantics in the binding slice.
7. **Promote indexes only after provenance is stable.** Derived timeline/search
   indexes can be persisted later, but every row must point back to
   `runtime_source_uri` and `source_cursor`.

## Surface contracts

### PeerRegistry metadata

`SessionMapping` remains the compatibility source for peer reuse. It is stored
in SQLite, with one-time import from legacy `sessions.json`. The legacy JSON
file remains a downgrade/export artifact, not the active daemon write target.

Peer metadata may temporarily carry `repowire_session_id`, `runtime_session_id`,
or source locator hints. Treat metadata as an ingress/compatibility envelope,
not as the final durable schema.

### Hooks

SessionStart should create or update the binding edge between project path,
backend, pane, current peer, and any runtime session hint it has. Stop should
advance `last_seen_at`, `runtime_session_id`, source cursor, and turn
provenance. Hooks must keep parsing raw source data locally and post only the
existing chat/event payloads plus binding pointers added by future slices.

### Timeline and transcript routes

Timeline/transcript routes should use bindings to avoid path-only discovery
when a runtime session is known. They should still be able to rebuild from raw
runtime history using `runtime_source_uri` and `source_cursor`.

The current optional `session_id` query parameter means backend runtime
session ID. A future route should prefer `repowire_session_id` for durable
workstream targeting and reserve `runtime_session_id` for source filtering.

### Asks, approvals, and receipts

Ask/ack state remains lifecycle state, not transcript history. Future records
should add session pointers for audit and UI grouping:

- `from_repowire_session_id`
- `to_repowire_session_id`
- `from_peer_id`
- `to_peer_id`
- `correlation_id`
- `delivery_state`
- `delivery_receipt_provenance`

Approvals should follow the same pattern: store control/provenance state and
decision metadata, not raw runtime transcript bodies. Delivery receipts should
distinguish daemon acceptance, transport delivery, runtime pickup when known,
and user/agent acknowledgement when known.

### Resume and session controls

Resume controls should target `repowire_session_id`. The binding then decides:

- whether an active executor can receive the command;
- whether the backend supports resume;
- which runtime session/source locator should be passed to the backend;
- whether the session is detached, resumable, archived, lost, or superseded.

If a session has no reliable runtime source locator, resume should fail with a
clear capability error rather than guessing from display name or current
working directory alone.

Current compatible slice:

- `POST /sessions/{repowire_session_id}/controls/notify` resolves an active
  executor from the binding and uses the existing notify delivery path.
- `POST /sessions/{repowire_session_id}/controls/resume` reports
  `active_executor`, `resume_available`, or `unsupported` capability status.
  Passing `dry_run=false` executes backend-specific resume through the daemon
  spawn service when the binding is resumable.

Peer-targeted controls remain the compatibility surface while session-targeted
routes harden.

## SQLite fit

Session bindings fit the daemon-owned SQLite boundary better than raw
transcripts because they are control/provenance state. The current binding table
lives under the existing `StateDatabase` lifecycle and remains distinct from
`PeerRegistry.SessionMapping` so peer identity restart behavior and downgrade
compatibility are preserved.

Do not use the binding table as a reason to remove peer/path compatibility,
rewrite delivery semantics, or persist raw runtime transcript bodies. Further
SQLite expansion should define store interfaces, migration tests, JSON
downgrade/backcompat behavior, and failure policy separately.

## Non-goals for this contract

- No dashboard UI changes.
- No changes to ask reminder, pending reply, TTL, or approval semantics.
- No model switching or production-ready ACP claim.
- No background polling. Binding repair should follow the existing lazy-repair
  philosophy.
