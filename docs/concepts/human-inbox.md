# Human inbox

The human is a peer. The daemon registers one human peer per user, and every message sent to that peer lands in a durable inbox. Telegram, the CLI, and (in a follow-up) the dashboard display that inbox. Senders address the human; they never choose a surface.

## Why

Before the inbox, a person on a phone could only be reached by answering a question they had asked. A `notify_peer('telegram', ...)` went to the Telegram bridge's socket. If the bridge's call to Telegram failed, the message was gone, and nothing in the ledger said so. The inbox removes that asymmetry. `notify_peer`, `ask`, `answer`, and `broadcast` reach the human the same way they reach any agent, and the sender gets a receipt.

## The human peer

| Property | Value |
| --- | --- |
| `display_name` | `human.name` from config (default `human`) |
| `role` / `backend` | `human` / `human` |
| Status | Always `online`: the inbox always accepts |
| Surface presence | `metadata.surfaces` (`{telegram: {live: true}}`) and `metadata.surfaces_live` |

The peer is registered in-process at daemon start, so liveness sweeps never demote it, and its `peer_id` stays stable across restarts. Presence is republished when a surface connects or drops; nothing polls.

## Surfaces are renderers

A surface is a client that displays the inbox and answers on the human's behalf:

- **Telegram** pushes every new item to the chat. A reply on a thread appears as a Telegram reply to the earlier message. See [Telegram](../use/features/telegram.md).
- **CLI**: `repowire inbox` lists, shows, marks read, and replies. See [CLI reference](../reference/cli.md#repowire-inbox).
- **Dashboard**: an inbox view is planned as a follow-up.

Read state is shared. Marking an item read on one surface marks it read everywhere. A reply from any surface acks the open ask behind it, or answers it for a structured question, and marks the item replied.

## Addressing and the `telegram` alias

`telegram` and `slack` (by name or by the bridge's `peer_id`) resolve to the human peer. Existing `notify_peer('telegram', ...)` callers keep working and now get receipts. In the other direction, a message the human types on Telegram comes from the human peer, so an agent's reply lands in the inbox and threads under the original message.

Frames the human peer sends carry `from_role: "human"`. Every renderer (Claude hooks and channel, Codex bridge, OpenCode, Pi) frames those as the user's direct instructions, whatever `human.name` is. The legacy names `dashboard`, `telegram`, `slack` and `human` keep that framing too. Renderers built before this change frame only those names, so set a custom `human.name` after updating them (`repowire setup` re-installs the plugins).

## Items

| Kind | Created by | Threaded |
| --- | --- | --- |
| `notify` | `notify_peer` / `POST /notify` to the human | No |
| `ask` | `ask` to the human | Carries its `correlation_id`; the ask stays open until the human replies |
| `answer` | An `ack`/`answer`/`decline` reply to an ask the human sent | Yes, under the human's original message |
| `broadcast` | `broadcast`, only when `human.broadcast: true` | No |

An item is threaded only when its `correlation_id` names an ask the daemon still tracks (open, or closed within the ask TTL) and the human is one of its parties. A plain notify never creates, acks, or closes an ask.

## Receipts and traces

The sender's synchronous receipt is the stored item and the surfaces a push was started on:

```text
notif-delivery-1a2b3c4d inbox_item=inb-5e6f7a8b surfaces=telegram
```

Pushes run in the background. Each surface acks its push, and the outcome is recorded on the message's own trace (the notify's `delivery_id`, or the ask's `correlation_id`). Stages:

| Stage | Detail |
| --- | --- |
| `inbox_stored` | `item_id`, `kind` |
| `surface_pushed` | `surface`, `item_id`, `verified` (false for a legacy bridge that cannot ack) |
| `surface_failed` | `surface`, `item_id`, `error_class` (`network`, `rate_limited`, `rejected`, `too_long`, `overloaded`, `no_receipt`, `other`) |
| `inbox_read` | `surface`, `item_id` |
| `inbox_replied` | `surface`, `item_id` |

These stages never carry message text; the text lives in the inbox. (An ask's existing `created` and `closed` rows keep their clipped text, as for any ask.) Routing replay counts `inbox_stored` as delivered.

## Broadcast and routing

Broadcasts are opt-in. With `human.broadcast: false` (the default), a broadcast never reaches the inbox or a phone. With it on, the item is stored, and each surface pushes it only if its own `human.surfaces.<name>.broadcast` is true.

A routing strategy never picks the human unless `routing.allow_human_targets: true`. Naming the human always works. See [Configuration](../reference/configuration.md#human).

## Failure behaviour

- A surface that is offline, slow, or failing never blocks the sender, daemon startup, or shutdown. The item is already stored, and the push records `surface_failed`.
- Shutdown cancels in-flight pushes and records them as failed before the store closes.
- The Telegram bridge retries network failures and rate limits a bounded number of times before it reports a failure. A network retry can duplicate a message whose response was lost.

## Related

- [Control surfaces](control-surfaces.md)
- [Message types](message-types.md)
- [MCP tools](../reference/mcp-tools.md)
- [HTTP API](../reference/http-api.md#human-inbox)
