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
  provide `hub.ExpireAttachments` as a pass; the registry provides `Emit`,
  so `retention_swept` reaches the journal and live subscribers like any
  other event.
- **Open asks:** the policy. An open ask still reads its peer message,
  inbox item, refs, attachments and trace, so `RetentionPolicy` never lets
  either class expire anything younger than the ask lifetime
  (`daemon.prune_max_age_hours`). Every table and the attachment files get
  this without a rule of their own, so config needs no floor; a shorter
  setting warns in `config check` and acts as the ask lifetime.

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
