# State and Storage

## What is stored

Repowire stores local config, daemon state, event history, attachments, schedules, jobs, and relay keys under the user's Repowire data paths.

## Config

`~/.repowire/config.yaml` contains daemon host/port/auth, spawn commands and profiles, delivery queue settings, relay config, and optional update checks.

## SQLite state

The daemon persists operational state in SQLite. It uses lazy repair and debounced writes where practical so normal mesh operations do not become eager polling or disk-churn loops.

Schema version 16 adds the [human inbox](../concepts/human-inbox.md): `inbox_items` (one row per item, with read and replied state) and `inbox_pushes` (one outcome per item and surface), and `inbox_refs` (every surface message an item was rendered as, so a reply to any of them finds it). The inbox stages it writes to `delivery_traces` carry ids and error classes, never message text. A daemon refuses to open a database stamped with a newer schema, so once v16 has run, rolling back needs a v16-aware binary.

Schema version 17 adds [cross-machine circles](../concepts/peers-and-circles.md#circles-across-machines): `daemon_meta` (the database's `store_epoch`, so another machine can tell a replaced database from one that lost nothing), `remote_inbound` and `remote_fences` (requests other machines sent here, and ids read before they arrived), and `remote_outbound` (requests sent from here, with their immutable text and sender). Rows are deleted 7 days after creation: they are protocol state, so this window is fixed.

## Attachments

Uploaded files are stored locally and expire with other content (below). Dashboard uploads can be tunneled through relay, but the final readable path is local to the daemon machine.

## Retention

One policy, `retention` in [configuration](../reference/configuration.md#retention), covers every store. It has two classes:

| Class | Default | What | At expiry |
| --- | --- | --- | --- |
| content | 7 days | text a peer or the human wrote: `peer_messages` bodies, ask text and replies in `delivery_traces`, message text in `events` (`notification`, `broadcast`, `ask`, `query`, `response`, `ask_delegated`), `chat_turn` transcripts, inbox item text, attachment files | removed in place (JSON gains `"content_expired": true`); rows and files that are only content are deleted |
| records | 180 days | `delivery_traces` and `events` rows, inbox items with their pushes and refs, terminal jobs, completed operations, expired runtime certificates (each peer keeps its newest) | deleted |

Not covered, because they bound live state rather than history: open jobs, schedules and calendar entries, peer mappings, session bindings, retirement records (72 hours), queued deliveries (`daemon.delivery_queue_ttl_seconds`), the in-memory ask tracker (`daemon.prune_max_age_hours`), the 500-event dashboard window, and cross-machine records (7 days, above). Log files are written by the service manager and rotate separately.

Each table declares its retention, or why it is exempt, next to its own queries in `daemon-go/state`; a test fails for a table with no declaration. The state store runs the sweep when the daemon starts and then at most once an hour, after a journal write finds one due. It writes in batches of 500 rows, records a `retention_swept` event with per-rule counts, and stops a sweep in flight when the daemon shuts down. An unreplied ask's inbox item is kept while the ask may be open. Attachment files expire when the attachment routes next upload or read one. Ask history (routing's per-peer ask counts) reads peer ids, not text, so stripping leaves it intact; deleting records makes it cover the last `records_days`. SQLite reuses the freed pages but does not shrink the file; to shrink it, stop the daemon and run `sqlite3 ~/.repowire/state.db VACUUM`.

**Upgrading to a release with retention.** The first daemon start on it sweeps at once, and a sweep cannot be undone: message text older than 7 days and records older than 180 days are gone, including the old ask text `repowire route reconstruct` reads and the oldest asks behind routing's per-peer ask counts. Before restarting on the new binary:

1. Back up the database with SQLite's online backup, which is consistent while the daemon runs: `sqlite3 ~/.repowire/state.db ".backup '$HOME/.repowire/state.db.pre-retention'"`.
2. Set `retention` in `config.yaml` if the defaults are not what you want (`0` keeps a class forever), and confirm with `repowire config check`.

After the restart, the `retention_swept` event in the journal records what the sweep removed.

## Related

- [Configuration](../reference/configuration.md)
- [Attachments](../use/features/attachments.md)
- [Lazy repair](../concepts/lazy-repair.md)
