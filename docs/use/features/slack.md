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

The bot uses the daemon's normal peer registration, ask, notify, and event routes. Messages from `@slack` are framed as human input to the receiving agent, and human inbound Slack messages open tracked asks by default.

## Limits

- The bot watches one configured channel. Messages in other channels are ignored.
- Multi-user control is possible: anyone with access to the channel can drive the mesh.
- Socket Mode uses an outbound WebSocket to Slack; the app-level token has broad authority and should be treated like the bot token.
- Slack file relay is not currently supported. Photos and documents posted in Slack are not downloaded and forwarded to agents.

## Troubleshooting

- Bot connects but ignores messages: check `SLACK_CHANNEL_ID` matches the channel you are typing in.
- Socket Mode URL fetch failed: the app token is wrong or missing `connections:write`.
- Bot can read but cannot post: the bot token is missing `chat:write` for the channel.

## See also

- [Control surfaces](../../concepts/control-surfaces.md)
- [Attachments](attachments.md)
- [Mobile mesh management](../workflows/mobile-mesh.md)
