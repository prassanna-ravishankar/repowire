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

`0` keeps a class forever; otherwise 7 to 36500 days. The 7-day floor
outlives an open ask (24 hours by default), whose inbox item and refs carry
the human's reply. Any other value warns and keeps the default.
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

## Sweeper

`state.Store.Sweep(ctx, policy, now)` runs the rules above, records first,
in batches of 500 rows per statement, and returns per-rule counts.
`service.Retention.MaybeSweep` runs it off the request path, at most hourly
and one at a time, from `Registry.LazyRepair` and once at daemon start. A
sweep that changed anything journals one `retention_swept` event with the
counts. The three earlier ad-hoc sweeps are removed.

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
