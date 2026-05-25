# Tracked work lifecycle

Status: architecture contract plus first daemon skeleton for tracked work. The
daemon now has a durable tracked-work store and HTTP status/result/cancel
surface separately from conversational `ask`/`ack`. Executor delivery,
dashboard workflows, ACP/channel health handling, transport cancel, and backend
resume execution are still future slices.

## Problem

`ask` is a conversation thread: one peer asks another peer something, and the
recipient closes the thread with `ack`. That is useful for collaboration, but it
is not enough for durable work items that need explicit ownership, status,
progress, result data, cancellation, retention, and visibility across session
reattachments.

Tracked work is the daemon-owned lifecycle for those durable work items. A
tracked work item may originate from a CLI command, MCP tool, dashboard action,
Telegram/Slack command, orchestrator workflow, schedule, or future session
command. Once accepted, its lifecycle is represented by daemon state, not by
whether one chat message has been acknowledged.

## Boundary from ask/ack

Tracked work and ask/ack must remain separate primitives:

- `ask` opens a non-blocking conversational thread and returns a
  `correlation_id`; `ack` closes that thread.
- Tracked work opens a daemon work record and returns a `work_id`; status is
  read and changed through work lifecycle APIs.
- An ask may create, reference, or comment on tracked work, but acking the ask
  does not complete the work.
- A tracked work item may emit notifications or asks for human input, but those
  asks are child communication events, not the source of truth for the work
  state.
- Ask reminder, pending reply, TTL, and reply-routing behavior remain owned by
  `AskTracker`.

## Current daemon skeleton

The first shipped daemon slice provides a neutral tracked-work record and HTTP
API. It is intentionally not tied to Anya, a default orchestrator persona, or a
specific backend.

- `POST /jobs` / `POST /work` creates a durable work record and returns a
  `job_id` / `work_id` with initial `queued` status.
- `GET /jobs` / `GET /work` lists status records with optional filters for
  state, owner, creator, Repowire session, and circle.
- `GET /jobs/{job_id}` / `GET /work/{work_id}/status` returns the current
  status read model.
- `PATCH /jobs/{job_id}` / `PATCH /work/{work_id}` updates lifecycle state and
  can append a progress note.
- `GET /jobs/{job_id}/result` / `GET /work/{work_id}/result` returns terminal
  result data, or
  `result_state=not_ready` plus status while work is non-terminal.
- `POST /jobs/{job_id}/cancel` / `POST /work/{work_id}/cancel` records an
  audit-visible cancel request.

The record includes job-facing and owner/source/scope fields such as `title`,
`kind`, `created_by_peer_id`, `owner_peer_id`, `assigned_peer_id`,
`source_kind`, `source_id`, `correlation_id`, `scope`, `circle`,
`repowire_session_id`, `visibility`, and progress events. This API is a
lifecycle foundation: it does not select executors, deliver work to transports,
cancel live runtime sessions, expose MCP job tools, or update
dashboard/Telegram/Slack UI yet.

Use jobs when work needs durable status, progress history, result metadata, or
cancellation. Use `ask` for a conversational request that another peer should
close with `ack`. Use `schedule` for future delivery of a notify or ask.

Terminal jobs cannot be moved back to non-terminal states in this slice. A
terminal job may be updated with the same terminal state to add bounded
metadata or progress notes, and omitted result fields preserve their existing
values.

## State model

The daemon should expose these states as the canonical tracked-work lifecycle:

| State | Meaning | Terminal |
| --- | --- | --- |
| `queued` | The daemon accepted the work, stored it, and has not yet selected or reached an executor. | No |
| `delivered` | The daemon delivered the work request to the selected peer/session/transport, but the executor has not reported active execution. | No |
| `running` | The executor has accepted the work and is actively working. | No |
| `awaiting_input` | Execution is paused waiting for user, peer, approval, credential, or external input. | No |
| `completed` | The work finished successfully and result metadata is available. | Yes |
| `failed` | The work ended with an error result. | Yes |
| `cancelled` | Cancellation was requested and the daemon reached a cancellation boundary. | Yes |
| `blocked` | The work cannot make progress without a new decision or dependency that is not merely ordinary user input. | No |
| `expired` | The work exceeded its TTL, deadline, or retention policy before completion. | Yes |
| `unavailable` | The target session, executor, backend, or required capability is unavailable before execution can continue. | Yes |

State transitions should be monotonic except for explicitly documented repair
paths. Lazy repair may move non-terminal work to `unavailable`, `expired`, or
another more accurate state when a user-visible request discovers stale daemon
state. It must not rely on polling.

## Status contract

`status` is the read model for work lifecycle state. It should be cheap to read
and stable enough for agents, dashboard views, and scripts.

Recommended fields:

| Field | Purpose |
| --- | --- |
| `job_id` / `work_id` | Daemon-generated stable identifier. |
| `title` | Short human-readable job title. |
| `kind` | Small type label such as `verification`, `research`, or `handoff`. |
| `state` | One state from the lifecycle table. |
| `state_reason` | Short machine-readable reason such as `executor_busy`, `permission_required`, `deadline_elapsed`, `capability_missing`, or `cancel_requested`. |
| `phase` | Optional executor-defined phase label for display. |
| `progress` | Optional bounded progress object, for example `{"current": 2, "total": 5, "unit": "checks"}`. |
| `progress_events` | Bounded operator history of progress notes and state observations. |
| `owner_peer_id` | Peer that owns or last owned execution, when known. |
| `assigned_peer_id` | Peer assigned to execute the job, when known. |
| `repowire_session_id` | Durable session/workstream binding when known. |
| `correlation_id` | Related ask/query correlation id, when known. |
| `circle` | Visibility and routing scope. |
| `created_by_peer_id` | Peer or service that created the work. |
| `created_at` | Daemon acceptance time. |
| `updated_at` | Last daemon-observed lifecycle update. |
| `deadline_at` | Optional deadline used for expiry. |
| `expires_at` | Optional retention or auto-expiry boundary. |
| `result_summary` | Small display summary for terminal work. |
| `links` | Related ask IDs, schedule IDs, event IDs, or session timeline pointers. |

Status reads must distinguish "no such work" from "work exists but is not
visible to this caller." The API may return a generic not-found result to
callers outside the visibility boundary, but internal audit logs should preserve
the difference.

## Result contract

`result` is available only for terminal work. Non-terminal result reads should
return the current `status` plus a clear `result_state` such as `not_ready`.

Recommended terminal result fields:

- `work_id`
- `state`: `completed`, `failed`, `cancelled`, `expired`, or `unavailable`
- `summary`: short human-readable outcome
- `data`: small structured payload for machine consumers
- `error`: structured error object for `failed` and relevant `unavailable`
  results
- `artifacts`: pointers to files, attachments, logs, branches, PRs, or timeline
  events
- `completed_at`
- `provenance`: source events, executor peer/session, and transport receipt
  pointers that explain how the daemon observed the terminal state

Result payloads should stay bounded. Large logs, transcripts, diffs, and
artifacts should be stored as external artifacts or provenance pointers, not
inline daemon state.

## Cancel semantics

Cancellation is a request first, then a terminal state after the daemon reaches
a defined boundary.

Expected behavior:

- `cancel(work_id)` records a cancel request even if the executor is not
  currently reachable.
- If work is still `queued`, cancellation can move directly to `cancelled`
  without contacting a transport.
- If work is `delivered`, `running`, `awaiting_input`, or `blocked`, the daemon
  should send the runtime/backend cancel instruction when the transport exposes
  one.
- A work item should report a pending cancel reason while cancellation is in
  flight, rather than claiming `cancelled` immediately.
- Terminal states win over late cancel requests. Cancelling `completed`,
  `failed`, `cancelled`, `expired`, or `unavailable` work should be idempotent
  and return the existing terminal status.
- Cancel requests must be audit-visible even when best-effort transport cancel
  fails.

## Protocol cancel before transport teardown

When the daemon or adapter needs to tear down a transport for a live tracked
work item, protocol-level cancel must be attempted before closing the transport
whenever the connection is still usable.

Required order:

1. Mark the work with `state_reason=cancel_requested` or equivalent pending
   cancel metadata.
2. Send the backend/runtime protocol cancel request, such as an ACP
   `session/cancel` equivalent for the active runtime session.
3. Wait only for the configured bounded acknowledgement window.
4. Close or tear down the transport if needed.
5. Move the work to `cancelled`, `failed`, or `unavailable` based on the best
   daemon-observed outcome.

If the connection is already broken, the daemon may skip the protocol cancel and
mark the work `unavailable` or `failed` with provenance showing that no
protocol-cancel attempt was possible. This contract defines ordering only; it
does not expand ACP/channel health diagnostics.

## Storage boundary

Tracked work is daemon control state and belongs under a daemon-owned store
interface. The first implementation may be in-memory for contract slicing, but
durable tracked work should persist through the existing daemon state boundary
used for schedules, session bindings, and events.

Persist:

- work identity, lifecycle state, timestamps, owner, creator, circle, and
  session pointers;
- small status/progress/result metadata;
- cancel requests and terminal outcomes;
- provenance pointers to asks, schedules, session events, runtime sessions,
  transport receipts, artifacts, and logs.

Do not persist:

- raw runtime transcript bodies as authoritative work state;
- unbounded logs or full command output inline;
- backend secrets, approval credentials, tokens, or private transport handles;
- Beads ledgers or product-repo issue tracker data as part of work records.

The store should have an explicit retention policy for terminal work. Retention
cleanup must follow Repowire's lazy-repair philosophy and should be triggered by
user-visible requests, startup/shutdown, or bounded maintenance hooks, not a new
polling loop.

## Session and circle visibility

Tracked work is visible through both the durable session model and the mesh
circle model:

- `repowire_session_id` groups work with a durable workstream when known.
- `owner_peer_id` is the current or last executor; it may be absent for queued
  work or detached sessions.
- `circle` scopes default visibility and name resolution.
- Peers in the same circle may see work addressed to that circle according to
  role policy.
- Human/service/orchestrator peers that already bypass circle lookup may inspect
  work across circles only through explicit role policy, not by display-name
  guessing.
- Exact IDs override display names for routing and inspection. `work_id`,
  `peer_id`, and `repowire_session_id` should avoid ambiguous name lookup.

If a work item targets a detached or resumable session with no active executor,
it should remain `queued`, become `unavailable`, or report a clear capability
error. It must not silently fall back to a peer with the same display name or
working directory.

## Non-goals

- No changes to ask reminder, ack, pending reply, or ask TTL semantics.
- No ACP/channel broker health matrix or readiness dashboard.
- No Claude plugin packaging or marketplace behavior.
- No SQLite cleanup, migration consolidation, or broad state-store refactor in
  this design slice.
- No dashboard UI implementation.
- No graphify update requirement.
- No automatic Beads issue import/export or product commits containing Beads
  ledger churn.
