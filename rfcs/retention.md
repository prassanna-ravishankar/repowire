# Retention: one policy for everything the daemon keeps

Status: implemented (2026-11-24). A prerequisite for `wire-observability.md`,
which must not widen access to message bodies the daemon never forgets.

## Problem

Retention was a per-table afterthought. Three stores expired on their own
clocks (`peer_messages` 7 days, cross-machine records 7 days, attachments
24 hours), and the largest ones never expired: `delivery_traces` and
`events` hold ask text, replies and transcripts; `inbox_items` holds the
human's messages; `tracked_work`, `operations` and
`runtime_identity_certificates` grow forever. On the author's daemon
(2026-11-23): 36k events, 6k trace rows, 3.2k expired certificates, 96 MB.

## Decision

Two data classes, one sweeper.

```yaml
retention:
  content_days: 7     # text a peer or the human wrote, and attachment files
  records_days: 180   # the rows that say who did what, when, and how it ended
```

`0` keeps a class forever; a negative, non-numeric or over-36500 value
warns and keeps the default.
When `records_days` is shorter, content goes with its record.

| Store | At `content_days` | At `records_days` |
| --- | --- | --- |
| `peer_messages` | delete row | n/a |
| `delivery_traces` | remove `text`, `reply`, `note`, `request.text` from `detail_json` | delete row |
| `events` | `chat_turn`, `chat_turn_delta`: delete row. Message events: remove `text`, `message`, `note`, `question`, `attachments` | delete row |
| `inbox_items` | blank `text`, `attachments_json`, `question_json` | delete row, its pushes and refs |
| `tracked_work` | n/a (a job keeps its result) | delete terminal jobs |
| `operations` | n/a | delete completed operations |
| `runtime_identity_certificates` | n/a | delete rows expired before the cutoff, except each peer's newest (a runtime renews from it) |
| attachments directory | delete file | n/a |

Stripped JSON rows gain `"content_expired": true`, so `repowire why` says
the text expired rather than showing nothing.

Cross-machine records (`remote_*`) keep a fixed 7-day window
(`state.RemoteWindow`): a live remote ask is retried and answered from its
row, and fences guard against replays, so the policy must not shorten it.
The sweeper applies that window too.

Exempt, because they bound live state rather than history: open jobs,
schedules, identity and resume tables, queued deliveries, the in-memory ask
tracker, retirement records, the dashboard's 500-event window.

## Ownership

Each piece lives with what it expires, so its lifecycle has one owner.

- **Rules:** each table declares a `tableRetention` (its rules per class,
  or why it is exempt) in its own `state/<domain>.go` file.
  `TestEveryTableDeclaresRetention` reads `sqlite_master` and fails for a
  table without exactly one declaration, so a new table cannot silently go
  unswept.
- **Sweep lifecycle:** the store. `Store.StartRetention(policy, plugs)`
  sweeps at once; later sweeps start after an event write, at most hourly,
  one at a time, off the writer's path. A second start is refused.
  `Store.Close` cancels a sweep in flight and waits for it before closing
  the database. Sweeps run every table's records rules, then every content
  rule, in batches of 500 rows.
- **Plugs:** what the store does not hold plugs into the same sweep
  (`RetentionPlugs`). The attachment routes, which own the directory,
  provide `Hub.ExpireAttachments` as a pass; the registry provides `Emit`,
  so `retention_swept` reaches the journal and live subscribers like any
  other event. A file's life runs from its latest reference, not its
  upload (#573): the pass runs after the tables and keeps any file a stored
  message (`peer_messages`, `inbox_items`, `queued_deliveries`) still names
  by id or path (`Store.AttachmentReferences`). A file nothing names goes
  once its mtime passes the content cutoff. If the lookup fails, every file
  is kept.
- **Open asks:** the policy. An open ask still reads its peer message,
  inbox item, refs, attachments and trace, so `RetentionPolicy` never lets
  either class expire anything younger than the ask lifetime
  (`daemon.prune_max_age_hours`). Every table and the attachment files get
  this without a rule of their own, so config needs no floor; a shorter
  setting warns in `config check` and acts as the ask lifetime.

## Hosted

The hosted runtime follows the same policy (`hosted/src/retention.ts`).

- **Defaults:** one copy in `daemon-go/proto/retention.json`. The daemon's config embeds it, and the Worker imports it.
- **Policy:** the owner's, held by the Gateway under `retention:policy`, with the defaults until one is set. The data belongs to the namespace, so once accounts exist the policy is the account's, set at the relay and sent to the Gateway from there (`managed-accounts.md`). A Gateway from before managed agents were reached only through the relay kept one policy per connected daemon; the first policy read folds them into the owner's, the most conservative in each class (the longest age, with 0 winning, and the longest ask lifetime), so no deployed data goes sooner than it was kept under.
- **Rules:** each Gateway table declares its retention as a static field beside its queries. A test fails for a table without exactly one declaration.
  - `runs`: the answer is content; the settled row is a record.
  - `requests`: the text is content and the row is a record, both timed from when the request ended: its run settled, or it was settled as not delivered. A request with no settled run is kept, since its reader still reconciles it. The text is stripped only when a stored digest still matches a retry against its receipt.
  - `remote_*`: the fixed window.
  - `agents`, `workspaces`: exempt, as identity and live state.
  - `usage_event`, `usage_day`: exempt, as the billing ledger.
- **Sweep:** owned by the Gateway Durable Object. It runs at most hourly, when a report or a cross-machine request finds one due, in batches of 500 rows with a per-sweep budget. It never runs on a timer.
- **Each agent's own records** (`HostedAgent`). The per-operation dedupe markers `reported:<op>` and `undelivered:<op>` record when they were set. The agent sweeps them at most hourly, on its own submits, using the owner's policy from the Gateway.
  - They expire with records, never sooner than the cross-machine window or the ask lifetime, since a resend or read-back of the id can come until then.
  - With records kept forever, they are kept forever too.
  - `report:<op>` and `steps:<op>` are live until the report lands, and are removed then.
- **Retired agents and unused workspaces go whole** (#587). When the Gateway sweeps, it asks a few retired agents, and workspaces no live agent uses, to delete themselves. Each must be past the content age (no later than records), with the same floors as the markers.
  - An agent goes only with no open run, no open cross-machine record and nothing unreported. It stops its lifecycle and pi in memory, then deletes its whole Durable Object storage: conversation, operation records and markers. It keeps one fence, a configuration marked retired and purged, so the id never runs again: reads of it answer fenced or expired, and creating the id again is refused (410).
  - A workspace goes only while no container runs and no command ran since the cutoff, and deletes its saved copy of `/workspace` in R2 with it. The Gateway claims it first, and no agent can take it up until the deletion ends. (Before directory backups, workspaces were container snapshots, which the Worker could not delete; those expire on the platform within 30 days.)
  - The Gateway rows stay, marked `purged_at`, as identity. A workspace taken up again starts empty, and `/managed` shows a deleted conversation as deleted.
- **Not covered: a live agent's old transcript.** pi-durable keeps every entry by design (compaction and reset only hide old entries from the model), and it has no API to delete them. Deleting its rows underneath it would break its invariants, so live-agent trimming waits for an upstream API (drafted, held for Prass).

## Readers

- Routing's ask history reads peer ids, not text: stripping leaves it
  intact; deleting records makes it a rolling `records_days` window.
- `route reconstruct` reads old ask text from the journal. Anyone who needs
  that history archives `state.db` before the first sweep.
- `list_messages` and `read_message` lose a message after `content_days`.
- A reply to an inbox item whose text expired still finds it by ref.

## Compatibility

No schema change. The first sweep is irreversible; the upgrade notes in
`docs/operate/state-and-storage.md` say to back up `state.db` first.
Attachments move from 24 hours to 7 days, as long as the messages that name
them.
