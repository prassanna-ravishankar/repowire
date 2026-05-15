# Message types

The daemon routes four message types. Pick by lifecycle, not by content.

## `ask`

Non-blocking. Returns a `correlation_id` immediately. The recipient closes the thread with `ack(corr_id)` (bare) or `ack(corr_id, message)` (reply). Chain follow-ups with `ask(reply_to=corr_id, ...)`, which closes the prior thread and opens a new one referencing it.

If the recipient never acks, repowire injects a reminder block at the start of every subsequent prompt on the recipient side until the ask is acked. Tool-call detection is the source of truth — prose `[ack #cid]` mentions in agent output do not close anything, only a real `ack()` MCP call does.

## `ack`

Closes an open ask thread.

- Bare `ack(cid)` signals "seen, no action needed."
- Reply `ack(cid, message)` delivers the message back to the original asker as a notification framed `[ack #cid from @peer] message`.

Replies always reach the original asker regardless of circle — the thread was established at ask-time and the routing is locked then.

## `notify_peer`

Fire-and-forget. No lifecycle, no response expected. Returns a synthetic `notif-XXXXXXXX` ID for client-side tracking, not a thread you can close.

The special peer name `telegram` routes to the user's phone (if the Telegram bot is running). The dashboard already sees agent turns; you do not need to notify it.

## `broadcast`

Fan-out to all online peers in your circle. No correlation, no reply. Use sparingly — treat it as a soft interrupt for everyone in scope.

## Misroute refusal

If a `peer_name` matches multiple peers across different circles, `ask` and `notify_peer` refuse the call with a hint to disambiguate. Pass an explicit `circle=` argument to pick one. This prevents a silent wrong-peer delivery when display names collide.
