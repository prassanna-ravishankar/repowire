# Telegram

## What it is

The Telegram bot is a human control surface that registers as the `telegram` peer. Notifications agents send to it appear in your Telegram chat; messages you send route back into the mesh as tracked asks by default.

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
```

`telegram` is a canonical service address. A unique service/human display name resolves across circles, so agents do not need to discover or pass the bot's configured service circle. Ordinary agent names remain circle-scoped.

## Limits

- Telegram traffic is not proxied by the relay; the bot talks directly to Telegram's API and to your daemon.
- Telegram updates are acknowledged only after Repowire handles them successfully. A transient daemon or Telegram API failure is retried instead of silently consuming the command.
- Attachments live in `~/.repowire/attachments/` with a 24-hour TTL and 10 MB upload limit.
- Messages from `@telegram` are framed as human instructions to the receiving agent.

## Troubleshooting

- Bot does not respond to `/peers`: confirm `TELEGRAM_BOT_TOKEN` is set and the bot is registered with `@BotFather`.
- Messages route to the wrong peer: check the sticky target with `/peers`; `/clear` resets it.
- Photos do not reach the agent: confirm the daemon is reachable from the bot host and `/attachments` is not blocked by a proxy.

## See also

- [Attachments](attachments.md)
- [Control surfaces](../../concepts/control-surfaces.md)
- [Mobile mesh management](../workflows/mobile-mesh.md)
- [Diagnostic commands](../../troubleshooting/diagnostics.md)
