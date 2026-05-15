# Telegram bot

The Telegram bot is a peer. It registers as `telegram`. Notifications agents send to it appear in your Telegram chat; messages you send route back into the mesh.

## Setup

```bash
TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
```

Tokens can also live in `~/.repowire/config.yaml`:

```yaml
telegram:
  bot_token: "..."
  chat_id: "..."
```

`repowire setup` writes those keys after the interactive prompt for Telegram credentials. The bot can run on any machine reachable from the daemon; many people run it on the same box.

## Commands

| Command | What it does |
| --- | --- |
| `/peers` (or `/start`, `/list`) | Show online peers as inline buttons; tap to pick a notify target |
| `/select <peer>` | Sticky-route subsequent messages to that peer until `/clear` |
| `/switch <peer>` | Alias for `/select` |
| `/clear` | Clear the sticky target |
| `@peer message` | One-shot send to a specific peer (also updates sticky) |

## Sticky routing

After `/select repowire`, every message you type goes to `repowire` as a notification until you `/clear` or `/select` another peer. The bot displays the current sticky target in its reply keyboard so you can see where messages are going.

The reply keyboard shows up to three current peers and three recent peers, with markers indicating which is which.

## Human framing

Agents on the receiving end see `@telegram` as a human. Repowire injects a context line at message-arrival time telling the agent that `@telegram` is the user, not an agent — so the agent treats the message as a direct user instruction rather than agent-to-agent traffic.

To send something to your phone from an agent:

```python
notify_peer("telegram", "deploy finished, green across CI")
```

## Attachments

The bot downloads photos sent in Telegram, uploads them to the daemon via `POST /attachments`, and includes the resulting local path in the outgoing notification. The recipient agent can then read the image via its multimodal tool — Claude's `Read` tool, for instance, accepts the local path directly.

Attachments live in `~/.repowire/attachments/` with a 24-hour TTL.

## Self-host vs hosted

The bot ships in the same `repowire` package. There is no hosted Telegram bot — you bring your own bot via `@BotFather` on Telegram, configure the token, and run `repowire telegram start` somewhere. The relay does not proxy Telegram traffic; the bot talks directly to Telegram's API and to your local daemon.

## Troubleshooting

- Bot doesn't respond to `/peers` — confirm `TELEGRAM_BOT_TOKEN` is set and the bot is registered with `@BotFather`.
- Messages route to the wrong peer — check the current sticky target with `/peers`; `/clear` resets it.
- Photos don't reach the agent — confirm the daemon is reachable from the bot host and `/attachments` is not blocked by a proxy.
