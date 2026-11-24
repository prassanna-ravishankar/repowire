# State and Storage

## What is stored

Repowire stores local config, daemon state, event history, attachments, schedules, jobs, and relay keys under the user's Repowire data paths.

## Config

`~/.repowire/config.yaml` contains daemon host/port/auth, spawn commands and profiles, delivery queue settings, relay config, and optional update checks.

## SQLite state

The daemon persists operational state in SQLite. It uses lazy repair and debounced writes where practical so normal mesh operations do not become eager polling or disk-churn loops.

Schema version 16 adds the [human inbox](../concepts/human-inbox.md): `inbox_items` (one row per item, with read and replied state) and `inbox_pushes` (one outcome per item and surface), and `inbox_refs` (every surface message an item was rendered as, so a reply to any of them finds it). The inbox stages it writes to `delivery_traces` carry ids and error classes, never message text. A daemon refuses to open a database stamped with a newer schema, so once v16 has run, rolling back needs a v16-aware binary.

Schema version 17 adds [cross-machine circles](../concepts/peers-and-circles.md#circles-across-machines): `daemon_meta` (the database's `store_epoch`, so another machine can tell a replaced database from one that lost nothing), `remote_inbound` and `remote_fences` (requests other machines sent here, and ids read before they arrived), and `remote_outbound` (requests sent from here, with their immutable text and sender). Rows are deleted 7 days after creation: they are protocol state, so this window is fixed.

Schema version 19 adds `peer_wire_visibility`: each peer's chosen [wire visibility](../reference/mcp-tools.md#set_wire_visibility), kept like its message delivery mode: exempt from retention, and deleted with the delivery mode when the peer's id is closed for good (a hard retirement). From this version the stages that name a thread's parties (an ask's `created`, `delegated` and `reassigned`, a notify's `resolved_peer`) carry a `provenance` detail: each party's circle, role and visibility at that moment. Retention strips message text from trace details, never provenance.

## Attachments

Uploaded files are stored locally and expire with other content (below). Dashboard uploads can be tunneled through relay, but the final readable path is local to the daemon machine.

## Retention

One policy, `retention` in [configuration](../reference/configuration.md#retention), covers every store. It has two classes:

| Class | Default | What | At expiry |
| --- | --- | --- | --- |
| content | 7 days | text a peer or the human wrote: `peer_messages` bodies, ask text and replies in `delivery_traces`, message text in `events` (`notification`, `broadcast`, `ask`, `query`, `response`, `ask_delegated`), `chat_turn` transcripts, inbox item text, attachment files | removed in place (JSON gains `"content_expired": true`); rows and files that are only content are deleted |
| records | 180 days | `delivery_traces` and `events` rows, inbox items with their pushes and refs, terminal jobs, completed operations, expired runtime certificates (each peer keeps its newest) | deleted |

Hosted agents follow the same policy on the hosted service. The daemon sends it to the owner's Gateway on every gateway connect. If several of the owner's machines connect, the Gateway keeps each class as long as any of their policies asks, so no machine shortens retention for another. The Gateway sweeps its own tables at most hourly, when a report, a connect or a cross-machine request finds a sweep due. Run answers and relay request text are content; settled runs and requests are records; open runs and their requests stay whole. Usage rows are the billing ledger and are never swept, and cross-machine records keep the same 7-day window. Without a connected daemon, the defaults apply; both sides read them from `daemon-go/proto/retention.json`. Each hosted agent expires its own per-operation records by the same policy. Once past the content age, a retired agent is deleted whole, conversation included, and so is a workspace no live agent uses; a workspace's last snapshot expires on the platform within 30 days. A live agent's older transcript is kept, because its runtime cannot yet expire it.

Not covered, because they bound live state rather than history: open jobs, schedules and calendar entries, peer mappings, session bindings, retirement records (72 hours), queued deliveries (`daemon.delivery_queue_ttl_seconds`), the in-memory ask tracker (`daemon.prune_max_age_hours`), the 500-event dashboard window, and cross-machine records (7 days, above). Log files are written by the service manager and rotate separately.

Each table declares its retention, or why it is exempt, next to its own queries in `daemon-go/state`; a test fails for a table with no declaration. The state store runs the sweep when the daemon starts and then at most once an hour, after a journal write finds one due. Attachment files plug into the same sweep: a file is kept while a stored message still names it, and is removed once nothing names it and it is older than the content cutoff. It writes in batches of 500 rows, records a `retention_swept` event with per-rule counts (shown live on the dashboard), and stops a sweep in flight when the daemon shuts down. Nothing younger than the ask lifetime (`daemon.prune_max_age_hours`) expires, whatever the policy. Ask history (routing's per-peer ask counts) reads peer ids, not text, so stripping leaves it intact; deleting records makes it cover the last `records_days`. SQLite reuses the freed pages but does not shrink the file; to shrink it, stop the daemon and run `sqlite3 ~/.repowire/state.db VACUUM`.

**Upgrading to a release with retention.** The first daemon start on it sweeps at once: message text older than 7 days and records older than 180 days are removed from `state.db`, including the old ask text `repowire route reconstruct` reads and the oldest asks behind routing's per-peer ask counts.

Before that first sweep the daemon copies the whole database once, next to it, as `state.db.pre-retention-<UTC timestamp>`, and records the path in a `retention_backup` journal event. If the copy fails, nothing is swept (a `retention_backup_failed` event says why) and the daemon tries again at the next sweep. A copy taken before a schema migration (below) counts as this backup when no sweep has run yet, so an upgrade that both migrates and turns retention on makes one copy, not two.

**Copies before a schema migration.** When a new release opens a database at an older schema, it first copies the database as it was, next to it, as `state.db.pre-migration-v<from>-to-v<to>-<UTC timestamp>`, and journals a `state_snapshot` event. The copy keeps the old schema, so the previous release can be restored from it. If the copy cannot be taken, the daemon refuses to migrate and says why; set `REPOWIRE_SKIP_MIGRATION_COPY=1` to migrate without one. Copies are private (0600). The daemon records each copy it takes by its exact path, keeps the newest `retention.copies` of those (3 by default, `0` keeps all), and removes older ones when it takes a new one. It never removes the pre-retention backup (the only copy of history the first sweep removed), and never a file it did not create, whatever its name. To keep everything instead, set `retention.content_days: 0` and `retention.records_days: 0` in `config.yaml` before upgrading, and confirm with `repowire config check`.

After the first sweep, the `retention_swept` event in the journal records what it removed.

## Related

- [Configuration](../reference/configuration.md)
- [Attachments](../use/features/attachments.md)
- [Lazy repair](../concepts/lazy-repair.md)
