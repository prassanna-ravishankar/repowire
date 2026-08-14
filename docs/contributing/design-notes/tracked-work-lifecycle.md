# Tracked work lifecycle

Status: architecture contract plus first durable job-runner slice. The daemon
now has a durable tracked-work store and lifecycle surface separately from
conversational `ask`/`ack`, persisted execution spec, HTTP/CLI
create-run-retry-cancel surfaces, and a daemon `JobRunner` that dispatches work
via `ask`. Recurring durable jobs are represented as internal calendar
templates that materialize normal tracked-work child jobs. Dashboard workflows,
MCP mirroring of run/retry/recurrence, ACP/channel health handling beyond the
narrow cancel hook, and broader backend resume execution remain future slices.
Recurring Codex jobs can reuse a recorded runtime session when the daemon has
observed compatible resume metadata for the same calendar/path/backend context.

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

## Current daemon slice

The current daemon slice provides a neutral tracked-work record, HTTP API, CLI,
and daemon runner. It is intentionally not tied to Anya, a default orchestrator
persona, or a specific backend.

- `POST /jobs` / `POST /work` creates a durable work record, persists
  `request.execution`, and returns a `job_id` / `work_id` with initial
  `queued` status.
- `POST /jobs` with `cron` creates a recurring calendar template and returns a
  `cal-...` id. The template is not an executable work item; it materializes
  child `work-...` jobs as fires become due.
- `GET /jobs` / `GET /work` lists status records with optional filters for
  state, owner, creator, Repowire session, and circle.
- `GET /jobs/{job_id}` / `GET /work/{work_id}/status` returns the current
  status read model.
- `POST /jobs/{job_id}/run` ignores `due_at` and manually dispatches queued,
  failed, or unavailable work.
- `POST /jobs/{job_id}/retry` dispatches failed, unavailable, or delivered
  work and preserves previous attempt records. Delivered retry is an explicit
  operator recovery path for a worker that received the ask but died before
  reporting status.
- `PATCH /jobs/{job_id}` / `PATCH /work/{work_id}` updates lifecycle state and
  can append a progress note. Runner-managed terminal updates must include the
  current `attempt_id`; stale attempt ids return conflict.
- `GET /jobs/{job_id}/result` / `GET /work/{work_id}/result` returns terminal
  result data, or
  `result_state=not_ready` plus status while work is non-terminal.
- `POST /jobs/{job_id}/cancel` / `POST /work/{work_id}/cancel` records an
  audit-visible cancel request.
- MCP tools `job_create`, `job_list`, `job_status` / `job_show`, `job_update`,
  `job_result`, and `job_cancel` wrap the same `/jobs` API and return JSON
  strings for agent callers.

The record includes job-facing and owner/source/scope fields such as `title`,
`kind`, `created_by_peer_id`, `owner_peer_id`, `assigned_peer_id`,
`source_kind`, `source_id`, `correlation_id`, `scope`, `circle`,
`repowire_session_id`, `visibility`, and progress events. Execution metadata is
persisted under `request.execution` with prompt, target, schedule, and delivery
subkeys. Runner provenance is persisted under `provenance.runner`, including the
current attempt id, lease, attempt count, and bounded attempt records. The
runner uses wake-on-create plus sleep-until-next-due behavior; it does not scan
continuously when no job is due.

Recurring templates store the same execution spec plus cron metadata. The
template controls recurrence state (`active` or `cancelled` in this slice),
`next_due_at`, and the last child id. Each child occurrence owns execution
state, attempts, result, retry, and cancellation. If the daemon was down for
several matching cron times, the next runner tick creates one overdue child and
advances the template to the next future fire.

Executor selection is deliberately small: an exact assigned peer id wins; an
ambiguous display name is rejected before create/run; otherwise `path` +
`backend` + optional `profile` follows the execution policy. Persistent jobs
may reuse a live peer for the same path/backend/circle. Per-fire jobs require
SessionControl to acquire a releasable executor, may use a recorded
backend-native resume binding for the same recurring calendar context, and
release that executor after terminal completion or compatible cancellation.
Delivery uses `ask` and records a correlation id. Ack confirms only receipt.
The worker prompt requires an immediate `state=running` update with the current
attempt id before longer work, then a terminal lifecycle update with the same
attempt id on completion. If a delivered job never sends that start heartbeat
before its dispatch lease expires, the runner may mark it unavailable so it can
be retried or inspected.

Use jobs when work needs durable status, progress history, result metadata, or
cancellation. Use `ask` for a conversational request that another peer should
close with `ack`. Use `schedule` for future delivery of a notify or ask.
Use recurring jobs when the future action is durable work, not merely a future
message.

Terminal jobs cannot be moved back to non-terminal states in this slice. A
terminal job may be updated with the same terminal state to add bounded
metadata or progress notes, and omitted result fields preserve their existing
values.

## State model

The daemon should expose these states as the canonical tracked-work lifecycle:

| State | Meaning | Terminal |
| --- | --- | --- |
| `queued` | The daemon accepted the work, stored it, and has not yet selected or reached an executor. | No |
| `dispatching` | A runner atomically acquired the work and is within its visible dispatch lease. | No |
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
  one. The current implementation attempts this only for an already-live
  daemon-owned ACP client for `assigned_peer_id`; it does not spawn a client or
  infer a runtime session from display name, path, or circle.
- A work item should report a pending cancel reason while cancellation is in
  flight, rather than claiming `cancelled` immediately.
- Terminal states win over late cancel requests. Cancelling `completed`,
  `failed`, `cancelled`, `expired`, or `unavailable` work should be idempotent
  and return the existing terminal status.
- Cancel requests must be audit-visible even when best-effort transport cancel
  fails.

Status includes `protocol_cancel` when a non-queued cancel request reaches the
protocol-cancel adapter. Values such as `status=sent` mean a bounded ACP
`session/cancel` was attempted. Values such as `status=unavailable` mean the
daemon had no live execution/session link to cancel and the work remains in the
pending-cancel contract until a later executor update or follow-up slice adds a
real execution binding.

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

Tracked work is daemon control state and belongs under a daemon-owned SQLite
store, alongside schedules, session bindings, and events. Recurring calendar
templates use the same state database and materialize ordinary tracked-work
children.

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

## Agent-folder convention

Recurring jobs can target a stable worker folder with `--path` and `--backend`.
Repowire does not maintain an agent registry in this slice. Use
`repowire agents create <name>` to scaffold `.repowire/agents/<name>` with
`AGENTS.md` as the source of truth and `CLAUDE.md` as a symlink for Claude Code.
Other supported runtimes load `AGENTS.md` directly. Store credentials outside
the job record and folder instructions; a job only spawns and prompts the
worker.

For recurring Codex jobs, each occurrence records executor provenance including
the peer id, backend, path, circle, tmux handles, runtime session id when the
runtime exposes one, and any observed Repowire session binding. The calendar
keeps the latest runtime binding plus a bounded history. On a later occurrence,
the runner reuses an already-live matching peer first; if none exists and the
latest binding advertises a compatible Codex resume capability, it launches
`codex resume <runtime-session-id>` through the normal spawn guardrails. Job
history remains audit/fallback context, not the primary resume mechanism.

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
- No automatic Beads issue import/export or product commits containing Beads
  ledger churn.
