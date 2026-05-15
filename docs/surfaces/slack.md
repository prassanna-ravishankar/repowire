# Slack bot

The Slack bot registers as the `slack` peer and connects to Slack over [Socket Mode](https://api.slack.com/apis/socket-mode), so no public URL or webhook is required.

## Setup

```bash
SLACK_BOT_TOKEN=xoxb-...   \
SLACK_APP_TOKEN=xapp-...   \
SLACK_CHANNEL_ID=C...      \
  repowire slack start
```

Or in `~/.repowire/config.yaml`:

```yaml
slack:
  bot_token: "xoxb-..."
  app_token: "xapp-..."
  channel_id: "C..."
```

You need a Slack app with:

- **Bot token** (`xoxb-...`) — scopes for `chat:write`, `channels:history`, `groups:history` as appropriate.
- **App-level token** (`xapp-...`) — `connections:write` scope. This is what Socket Mode requires.
- **Channel ID** (`C...`) — the channel the bot watches. Messages in other channels are ignored.

## Usage

The bot watches one channel. In that channel:

| Input | What happens |
| --- | --- |
| `select <peer>` or `switch <peer>` | Sticky-route messages to that peer |
| `@peer message` | One-shot send |
| Plain text after `select` | Routes to the sticky target |
| Tap a Block Kit peer button | Equivalent to `select` |

The bot posts a Block Kit message with peer buttons on demand, similar to the Telegram inline keyboard.

## Why Socket Mode

Socket Mode means the bot opens an outbound WebSocket to Slack — Slack doesn't need to reach you. No port forwarding, no Cloudflare tunnel, no public URL. The trade-off is that the app-level token has broad authority; treat it like the bot token.

## Human framing

Messages from `@slack` are framed as human input to the receiving agent, exactly like `@telegram` and `@dashboard`. Agents treat them as direct user instructions.

## Differences from the Telegram bot

- One channel, not one chat. Multi-user is possible — anyone with channel access can drive the mesh.
- Block Kit buttons instead of inline keyboards.
- No attachment relay yet. (Photos posted in Slack are not currently downloaded and forwarded to agents.)

## Troubleshooting

- Bot connects but ignores messages → check `SLACK_CHANNEL_ID` matches the channel you're typing in. Messages outside that channel are dropped silently.
- "Socket Mode URL fetch failed" → the app token is wrong or missing the `connections:write` scope.
- Bot can read but can't post → the bot token is missing `chat:write` for the channel.
