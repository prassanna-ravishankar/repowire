# Retention: one policy for everything the daemon keeps

Status: implemented on `feat/retention` (draft PR, 2026-11-23). Lands after the 2026-12 rollout. A prerequisite
for `wire-observability.md`, which must not widen access to message bodies
the daemon never forgets.

## Problem

Retention is a per-table afterthought. Three stores expire on their own
clocks, the largest ones never expire, and message text sits in five
places:

| Store | Holds text written by a peer or the human | Today |
| --- | --- | --- |
| `peer_messages` | message bodies | 7 days, hourly sweep on insert (`service/messages.go`) |
| `remote_inbound`, `remote_outbound`, `remote_fences` | cross-machine ask text and replies | 7 days, hourly sweep on a remote request (`hub/remote.go`) |
| attachments directory | uploaded files | 24 hours, swept on upload (`hub/routes_attachments.go`) |
| `delivery_traces` | ask text on `created`, reply on `closed` | never pruned |
| `events` | `notification`, `broadcast`, legacy `ask` and `query` text; `chat_turn` and `chat_turn_delta` transcripts | never pruned |
| `inbox_items` (+ `inbox_pushes`, `inbox_refs`) | the human's messages and replies | never pruned |
| `tracked_work` | job requests and results | never pruned |
| `operations` | restart and spawn attempts (no message text) | never pruned |
| `runtime_identity_certificates` | none; expired after 24h | never deleted |

On the author's daemon (2026-11-23): 36k events from 2026-05-22, 6k trace
rows, 1.7k jobs, 3.1k operations, 3.2k certificates (3.2k expired), a
96 MB database.

The following are lifecycle windows, not retention, and stay as they are:
the in-memory ask tracker (`daemon.prune_max_age_hours`, 24h), the
delivery queue (`daemon.delivery_queue_ttl_seconds`), retirement records
(72h, an identity-safety window), the 500-event dashboard window, spawn
ownership records (dropped when the pane dies), and cross-machine records
(7 days). Each one bounds live state for correctness; changing it changes
behaviour, not storage.

Cross-machine records are the subtle one. A remote ask is retried, read
back and answered from its row until 7 days pass (`hub/remote_source.go`),
and the fence table guards against replays within that window. Stripping
text from a live row would break a retry, and deleting a fence early would
weaken replay protection. So the window stays fixed at 7 days
(`state.RemoteWindow`), whatever the policy says, and the one sweeper
applies it.

## Decision

Two data classes and one sweeper.

```yaml
retention:
  content_days: 7     # text a peer or the human wrote, and attachment files
  records_days: 180   # the rows that say who did what, when, and how it ended
```

`0` means keep forever. Both are read like every other config key: a
mistyped value warns and keeps its default (`config check` names it).
Environment overrides: `REPOWIRE_RETENTION__CONTENT_DAYS`,
`REPOWIRE_RETENTION__RECORDS_DAYS`.

**Content** is any text a peer or the human authored: message bodies,
replies, delegation notes, decline reasons, question prompts, attachment
lists, transcripts, and attachment files. When content expires it is
stripped in place and its row is kept. If the row is pure content (a
`peer_messages` body, a `chat_turn`, an attachment file), the row or file
is deleted.

**Records** are the metadata: ids, peers, stages, states, timestamps, close
reasons, counts. When a record expires its row is deleted. If
`records_days` is shorter than `content_days`, content effectively expires
with the record.

### Per store

| Store | At `content_days` | At `records_days` | Age column |
| --- | --- | --- | --- |
| `peer_messages` | delete row | n/a | `created_at` |
| `delivery_traces` | remove `text`, `reply`, `note`, and `request.text` (the routing input's copy of the ask) from `detail_json` | delete row | `ts` |
| `events` | `chat_turn`, `chat_turn_delta`: delete row. `notification`, `broadcast`, `ask`, `query`, `response`, `ask_delegated`: remove `text`, `message`, `note`, `question`, `attachments` from `payload_json` | delete row | `timestamp` |
| `inbox_items` | `text = ''`, `attachments_json = '[]'`, `question_json = NULL` | delete row and its `inbox_pushes`, `inbox_refs` | `created_at` |
| `remote_inbound`, `remote_outbound`, `remote_fences` | (fixed 7-day protocol window, above) | delete row | `created_at` (unix) |
| `tracked_work` | n/a (see below) | delete terminal jobs | `completed_at`, else `updated_at` |
| `operations` | n/a | delete completed operations | `completed_at` |
| `runtime_identity_certificates` | n/a | delete rows that expired before the cutoff | `expires_at` |
| attachments directory | delete file | n/a | mtime |

A stripped field is removed rather than blanked where it lives in JSON, and
the row gains `"content_expired": true`, so readers can say "expired"
instead of showing an empty body. Inbox items keep their text in columns,
which are blanked (`text = ''`, `attachments_json = '[]'`,
`question_json = NULL`).

Route decisions keep their candidate snapshots, including each peer's
description. A description is authored text, but it is a status line
already shown by `list_peers`, not a message, and the decision benchmarks
read it.

Every timestamp column is ISO-8601 UTC, written as `...Z` or `...+00:00`
with or without fractions; rows are compared on the whole second, which
ignores the suffix.

**Jobs keep their content until the record goes.** A job's request and
result are the work product `job_result` exists to return, and the job is
created deliberately. Only terminal jobs (`completed`, `failed`,
`cancelled`, `expired`) are deleted; a live job is never touched.

**Exempt:** schedules and calendar entries (configuration, not history);
`peer_session_mappings`, `session_bindings`, `retired_peers`,
`peer_message_delivery` (identity and resume state); `queued_deliveries`
(its own delivery TTL); `daemon_meta`, `schema_migrations`,
`legacy_imports`. Log files (`daemon.log` is 113 MB on the author's machine)
are written by the service manager's redirect, not the daemon, so log
rotation is filed separately rather than forced into this sweeper.

### What changes for readers

- **Ask history** (`AskRefs`, routing counts) reads peer ids from trace
  `created` stages and legacy `ask` events. Stripping text leaves them
  intact; deleting records makes the counts cover the last `records_days`.
- **`repowire why` and `/traces/{id}`** show `content_expired` instead of
  the text after `content_days`.
- **`wait_on_ack` on a cross-machine ask** is unchanged: the fixed 7-day
  window still governs it.
- **`route reconstruct`** rebuilds routing inputs from old `ask` events,
  and `EventsOfTypes` deliberately keeps their text for it. Stripping that
  text changes what reconstruction can see. It already requires a copy of
  the database; anyone who needs that history copies `state.db` before the
  first sweep, or sets `content_days: 0`. The rollout runbook gains that
  step.
- **The dashboard timeline** reads `chat_turn` text from the in-memory
  500-event window, hydrated from the journal at startup. After a sweep
  the window may still hold older turns until it rolls over or the daemon
  restarts; after a restart it holds only what the journal kept.
- **Pull messages** (`list_messages`, `read_message`) lose a message after
  `content_days`, including its read state. A missing message is
  unavailable, not unread.
- **`job_list` and `job_show`** lose terminal jobs older than
  `records_days`.
- **The human inbox** keeps an item's sender, time and read or replied state
  for `records_days`, and its text for `content_days`. A reply to an
  item whose text expired still finds it by ref (the reply needs the
  correlation id, not the text). After `records_days` the item and its
  refs are gone, and a reply to it is treated as a reply to nothing.

## Sweeper

One `state.Store.Sweep(ctx, policy, now) (SweepReport, error)` runs every
rule in the table above. One `service.Retention` owns the policy, the last
sweep time, and a single-flight guard.

**When it runs.** Lazily, never on a timer: `Registry.LazyRepair` (already
triggered by requests, at most every 30 seconds) calls
`Retention.MaybeSweep`, which returns at once unless an hour has passed
since the last sweep, and otherwise starts the sweep on a tracked goroutine
so the request never waits. A daemon start counts as a request. The three
existing sweeps (`Messages.maybeSweep`, `remoteMesh.pruneLazily`,
`cleanupExpiredAttachments`) and their constants are removed.

**How it writes.** In batches of 500 rows per statement
(`... WHERE rowid IN (SELECT rowid ... LIMIT 500)`), looping until a batch
comes back short, so the first sweep over a large journal never holds the
writer for long. Each rule's condition stops matching a row once the rule
has run on it, so the loop ends. `events.timestamp` and
`delivery_traces.ts` are indexed; the tables without an index on their age
column (`inbox_items`, `tracked_work`, `operations`, certificates) hold a
few thousand rows, so no index or schema change is added. The database does
not shrink on disk without `VACUUM`; `repowire retention sweep --vacuum`
runs one on request.

**What it reports.** A sweep that changed anything appends one
`retention_swept` event with per-store counts (no ids, no text), and logs a
line. A failing store does not stop the others; the error is logged and
named in the report.

## Surface

- **Config:** `retention.content_days`, `retention.records_days`.
- **CLI:** `repowire retention` prints the policy, the last sweep, and what
  the next sweep would change per store (a dry run).
  `repowire retention sweep [--dry-run] [--vacuum]` runs one now.
  `repowire config check` prints the policy.
- **HTTP:** `GET /retention` (policy, last sweep, dry-run counts),
  `POST /retention/sweep` (`{"vacuum": bool}`). Both behind the daemon
  token. No MCP tool: retention is operator policy, not agent work.
- **Docs:** `reference/configuration.md` (`retention`),
  `reference/cli.md` (`repowire retention`), the HTTP reference,
  `operate/state-and-storage.md` (rewritten around the two classes), and
  the attachments and MCP message pages, and the cross-machine record
  window.

## Compatibility

- Defaults keep today's 7 days for message bodies and cross-machine text.
  Attachments move from 24 hours to 7 days, so a file lives as long as the
  message that names it.
- The first sweep on an existing daemon strips content older than 7 days
  from the journal and traces, and deletes records older than 180 days.
  It is irreversible; the runbook says to back up `state.db` first.
- No schema change, so rolling back to the previous binary needs no
  database restore (the sweep's deletions stay deleted).

## Tests

- Store: each row of the per-store table, at the boundary (one second
  either side of the cutoff); `0` keeps forever; `records_days` shorter
  than `content_days`; live jobs and unexpired certificates survive;
  batching (more than 500 eligible rows); mixed timestamp formats in
  `events.timestamp` (`...Z` and `...+00:00`).
- Readers after a sweep: `AskRefs` counts unchanged after stripping;
  stripped rows carry `content_expired`; route decisions keep their
  metadata and text hash.
- Remote records: a live row survives a policy shorter than the window; an
  expired one goes.
- Service: `MaybeSweep` runs at most hourly, single flight, off the
  request path; `Registry.Close` waits for an in-flight sweep.
- CLI and HTTP: dry run changes nothing; sweep reports counts; `--vacuum`.
- Config: defaults, env overrides, a mistyped value warns and keeps the
  default.

## Open questions

- `records_days: 180` makes routing's ask counts a rolling window. Keep
  forever instead (`0`), or is 180 days of history enough for routing?
- Should job content expire at `content_days` like messages? This draft
  keeps it with the record.
