# Mobile mesh management

Drive the mesh from your phone. The Telegram bot is the most common path; the mobile-responsive dashboard is the alternative.

## Telegram

Configure `telegram.bot_token` and `telegram.chat_id` in `~/.repowire/config.yaml`; setup installs the bot as an OS-managed user service alongside the daemon.

In your Telegram chat with the bot:

```text
/peers              # shows online peers as inline buttons
/select project-a   # all subsequent messages route to project-a
fix the failing CI on main, push when green
/clear              # stop sticky routing
```

Or one-shot without sticky routing:

```text
@project-a fix the failing CI on main, push when green
```

The bot registers as the `telegram` peer. Agents on the receiving side see your message framed as a human instruction, not as agent-to-agent traffic. Messages you type open tracked ask threads by default; use `/notify [@peer] message` or `/fyi [@peer] message` for fire-and-forget nudges.

## Dashboard

If you have the relay enabled, open `https://repowire.io/dashboard` on your phone browser. Same UI as desktop, hamburger menu for peer list, compose bar at the bottom. Works well for one-shots; for ongoing back-and-forth, Telegram is faster.

## The orchestrator hop

The pattern that scales: instead of talking to project peers directly, talk to an orchestrator that handles the dispatch.

```text
@orchestrator add a "rate limit the /search endpoint" ticket, dispatch when you have a slot
```

The orchestrator picks it up, decides which project peer to assign, dispatches, and reports back to you on Telegram when it lands. You stop being a router and start being the boss.

## Push notifications

The Telegram bot is a real Telegram bot, so push notifications work out of the box on iOS and Android. Agents calling `notify_peer("telegram", "deploy finished")` reach your phone.

## Attachments

Send a photo or document in the Telegram chat. The bot downloads it, uploads it to the daemon's `/attachments` endpoint (10 MB limit, 24 h TTL), and includes the local path in the resulting ask. The receiving agent reads the image via its multimodal tool (Claude `Read`, for example).

## When this isn't the right tool

- You need rich UI for the response. Telegram is plain text + buttons; the dashboard is better for chat threads with tool-call detail.
- You need to coordinate ten peers at once. The orchestrator pattern is the answer; Telegram is your interface to *the orchestrator*, not to the ten peers directly.

## See also

- [Telegram bot](../features/telegram.md) — full surface details.
- [Slack bot](../features/slack.md) — same pattern, different surface.
- [Web dashboard](../features/dashboard.md) — for when you want the full UI on mobile.
