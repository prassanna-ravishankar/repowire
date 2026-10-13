# State and Storage

## What is stored

Repowire stores local config, daemon state, event history, attachments, schedules, jobs, and relay keys under the user's Repowire data paths.

## Config

`~/.repowire/config.yaml` contains daemon host/port/auth, spawn commands and profiles, delivery queue settings, relay config, and optional update checks.

## SQLite state

The daemon persists operational state in SQLite. It uses lazy repair and debounced writes where practical so normal mesh operations do not become eager polling or disk-churn loops.

Schema version 16 adds the [human inbox](../concepts/human-inbox.md): `inbox_items` (one row per item, with read and replied state) and `inbox_pushes` (one outcome per item and surface), and `inbox_refs` (every surface message an item was rendered as, so a reply to any of them finds it). The inbox stages it writes to `delivery_traces` carry ids and error classes, never message text. A daemon refuses to open a database stamped with a newer schema, so once v16 has run, rolling back needs a v16-aware binary.

## Attachments

Uploaded files are stored locally with retention cleanup. Dashboard uploads can be tunneled through relay, but the final readable path is local to the daemon machine.

## Related

- [Configuration](../reference/configuration.md)
- [Attachments](../use/features/attachments.md)
- [Lazy repair](../concepts/lazy-repair.md)
