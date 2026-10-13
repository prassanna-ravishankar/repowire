# Telegram

## What it is

The Telegram bot is a surface of your [human inbox](../../concepts/human-inbox.md). Anything an agent sends you (a notify, an ask, a reply to your ask) is stored in the inbox and pushed to your Telegram chat. Messages you send route back into the mesh as tracked asks by default.

## When to use it

Use Telegram when you want phone-side mesh control, agent status updates, sticky routing to one peer, or image/document uploads from your phone.

Use the dashboard when you need a richer timeline or Jobs view. Use Slack when a team channel should drive the mesh.

## Setup

Create a Telegram bot token with `@BotFather`, identify the chat id Repowire should accept, then add both values to `~/.repowire/config.yaml`:

```yaml
telegram:
  bot_token: "..."
  chat_id: "..."
```

`repowire setup` writes those keys after the interactive prompt for Telegram credentials. When both are configured, setup installs the bot as an OS-managed user service alongside the daemon. There is no hosted Telegram bot; you bring your own bot. Use `repowire telegram start` only when intentionally running the bot separately on a machine that can reach the daemon.

## Common workflows

List and select peers:

```text
/peers
/select repowire
```

After `/select repowire`, every normal message opens a tracked ask to `repowire` until you `/clear` or select another peer. The reply keyboard shows display names for current and recent peers while routing internally by canonical `peer_id`.

Open an ask to a specific peer:

```text
@repowire status?
```

Send a fire-and-forget nudge:

```text
/notify @repowire deploy finished
/fyi logs are uploaded
```

Reply to an agent's message with Telegram's **Reply**: on an ask it acks the ask with your text; on anything else it opens a new ask to that agent. An agent's answer to your ask arrives as a Telegram reply under your original message. Asks carry **✓ Ack** or choice buttons; other messages carry **✓ Seen**, which marks the item read on every surface (Telegram gives bots no read signal of its own).

Send a photo when the target agent should inspect it. The bot uploads it to the daemon and includes it in the outgoing ask. Attachments sent by agents are uploaded back to Telegram as photos or documents.

## Commands and API

| Command | What it does |
| --- | --- |
| `/peers` (or `/start`, `/list`) | Show peers as inline buttons |
| `/select <peer>` | Sticky-route subsequent messages to that peer |
| `/switch <peer>` | Alias for `/select` |
| `/clear` | Clear the sticky target |
| `/keyboard on` / `/keyboard off` | Show or hide the persistent peer keyboard |
| `@peer message` | Open an ask to a specific peer and update sticky routing |
| `/notify [@peer] message` | Fire-and-forget notification; uses sticky target if omitted |
| `/fyi [@peer] message` | Alias for `/notify` |

Agents can reach your phone with:

```text
notify_peer("telegram", "deploy finished, green across CI")
# notif-delivery-1a2b3c4d inbox_item=inb-5e6f7a8b surfaces=telegram
```

`telegram` resolves to the human peer, so the message lands in your inbox even when the bot is down, and the sender gets a receipt. `repowire trace <delivery id>` shows whether the push reached Telegram (`surface_pushed`) or failed (`surface_failed` with `network`, `rate_limited`, `rejected`, `too_long`, `overloaded` or `no_receipt`). A unique service/human display name resolves across circles, so agents do not need to discover or pass the bot's configured service circle. Ordinary agent names remain circle-scoped.

## Limits

- Telegram traffic is not proxied by the relay; the bot talks directly to Telegram's API and to your daemon.
- Telegram updates are acknowledged only after Repowire handles them successfully. A transient daemon or Telegram API failure is retried instead of silently consuming the command.
- Attachments live in `~/.repowire/attachments/` with a 24-hour TTL and 10 MB upload limit.
- Messages from `@telegram` are framed as human instructions to the receiving agent.
- Mesh broadcasts reach Telegram only with `human.broadcast: true` and `human.surfaces.telegram.broadcast: true`.
- The bot retries network failures and rate limits up to three times per message. A retry after a lost response can deliver the same message twice.
- Long messages are split into chunks of about 3900 characters; a reply-to on the first chunk threads.
- Bot API errors are logged with the bot token redacted.

## Troubleshooting

- Bot does not respond to `/peers`: confirm `TELEGRAM_BOT_TOKEN` is set and the bot is registered with `@BotFather`.
- Messages route to the wrong peer: check the sticky target with `/peers`; `/clear` resets it.
- Photos do not reach the agent: confirm the daemon is reachable from the bot host and `/attachments` is not blocked by a proxy.

## See also

- [Attachments](attachments.md)
- [Control surfaces](../../concepts/control-surfaces.md)
- [Mobile mesh management](../workflows/mobile-mesh.md)
- [Diagnostic commands](../../troubleshooting/diagnostics.md)
