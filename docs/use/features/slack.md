# Slack

## What it is

The Slack bot is a human control surface that registers as the `slack` peer and connects to Slack over Socket Mode. No public URL or webhook is required.

## When to use it

Use Slack when a team channel should be able to select peers, send tracked asks, or send fire-and-forget FYIs into the mesh.

Use Telegram for personal phone control and file uploads. Use the dashboard for timelines, jobs, and richer inspection.

## Setup

Create a Slack app with:

- Bot token (`xoxb-...`) with `chat:write`, plus history scopes for the watched channel type.
- App-level token (`xapp-...`) with `connections:write`.
- Channel ID (`C...`) for the channel the bot watches.

For a one-off manual process, start the bot with:

```bash
SLACK_BOT_TOKEN=xoxb-... \
SLACK_APP_TOKEN=xapp-... \
SLACK_CHANNEL_ID=C... \
  repowire slack start
```

Or configure `~/.repowire/config.yaml`:

```yaml
slack:
  bot_token: "xoxb-..."
  app_token: "xapp-..."
  channel_id: "C..."
```

When all three values are configured, setup installs Slack as an OS-managed user service alongside the daemon. The manual command is only needed when running the bot separately from the daemon host.

## Common workflows

In the configured channel:

| Input | What happens |
| --- | --- |
| `select <peer>` or `switch <peer>` | Sticky-route messages to that peer |
| `@peer message` | Open an ask to a specific peer |
| Plain text after `select` | Open an ask to the sticky target |
| `notify [@peer] message` | Fire-and-forget notification; uses sticky target if omitted |
| `fyi [@peer] message` | Alias for `notify` |
| Tap a Block Kit peer button | Equivalent to `select` |

The bot posts peer picker buttons on demand, similar to the Telegram inline keyboard.

## Commands and API

Run:

```bash
repowire slack start
```

The bot uses the daemon's normal peer registration, ask, notify, and event routes. Messages from `@slack` are framed as human input to the receiving agent, and human inbound Slack messages open tracked asks by default. `slack` resolves to the [human inbox](../../concepts/human-inbox.md), so a message to `slack` is stored there even when the bot is down, and Slack is one of its surfaces (see below).

## Slack as an inbox surface

The bot declares itself an inbox surface, so the daemon pushes each [inbox](../../concepts/human-inbox.md) item to the channel and the sender gets a receipt. `repowire trace <delivery id>` shows whether the push reached Slack (`surface_pushed`, with the message `ts`) or failed (`surface_failed` with `network`, `rate_limited`, `rejected`, `too_long`, `overloaded` or `no_receipt`).

- **Threads.** Each new item starts its own thread. An item on the same ask as an earlier one (an agent's answer to your question, a follow-up) is posted in that item's thread.
- **Replies.** Reply in an item's thread to answer it. The reply goes through `POST /inbox/reply`: it acks the ask behind the item, marks the item read on every surface, and threads the agent's next answer under it. A reply in any other thread is an ordinary message to the sticky target.
- **Buttons.** A choice question shows its options as buttons, an acknowledge question one Ack button, and a tool approval adds Deny. A tap answers through the inbox, and the outcome is posted in the item's thread.
- **Quiet hours.** The bot declares `inbox_hold`, so `human.surfaces.slack.quiet_hours` holds its pushes in the configured window ([quiet hours](../../concepts/human-inbox.md#quiet-hours)).

No extra scopes are needed beyond the setup above. Thread replies arrive as ordinary channel `message` events, and buttons use the interactivity the peer picker already needs.

Unlike Telegram, Slack has no read sweep: a message you type in the channel does not mark earlier items read. Reply to an item or tap its button, or read it on the dashboard.

## Limits

- The bot watches one configured channel. Messages in other channels are ignored.
- Multi-user control is possible: anyone with access to the channel can drive the mesh.
- Socket Mode uses an outbound WebSocket to Slack; the app-level token has broad authority and should be treated like the bot token.
- The bot retries network failures and rate limits up to three times per message. A retry after a lost response can post the same message twice.
- Long items are split into chunks of about 3900 characters, all in the item's thread.
- Slack file relay is not currently supported. Photos and documents posted in Slack are not downloaded and forwarded to agents.

## Troubleshooting

- Bot connects but ignores messages: check `SLACK_CHANNEL_ID` matches the channel you are typing in.
- Socket Mode URL fetch failed: the app token is wrong or missing `connections:write`.
- Bot can read but cannot post: the bot token is missing `chat:write` for the channel.

## See also

- [Control surfaces](../../concepts/control-surfaces.md)
- [Attachments](attachments.md)
- [Mobile mesh management](../workflows/mobile-mesh.md)
