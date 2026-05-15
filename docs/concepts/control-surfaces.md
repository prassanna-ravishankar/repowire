# Control surfaces

The dashboard, Telegram bot, and Slack bot are peers too. They show up in `list_peers` alongside agents and use the same `ask` / `notify_peer` / `broadcast` primitives.

- `dashboard` — Next.js UI at `localhost:8377/dashboard`, with a live mesh log and per-peer chat. See [web dashboard](../surfaces/dashboard.md).
- `telegram` — bot you talk to from your phone. Sticky routing: `/select peer` sends subsequent messages to that peer until `/clear`. See [Telegram bot](../surfaces/telegram.md).
- `slack` — Socket Mode bot. Same sticky-routing pattern with Block Kit peer pickers. See [Slack bot](../surfaces/slack.md).

## Human framing

Messages from `@telegram` and `@dashboard` are humans. Agents that receive messages from these peers treat them as direct user instructions, not as agent-to-agent traffic. This framing is injected by repowire at delivery time — agents do not need to special-case the names themselves.

When an agent wants to reach the human, the move is `notify_peer("telegram", "...")` (or `notify_peer("dashboard", ...)`, though the dashboard already sees turns and rarely needs an explicit notification).

## What surfaces don't do

Control surfaces are clients of the routing API, not part of it. The daemon is the single source of truth for peer state and message routing. A surface that crashes does not lose mesh state; reopening it picks back up from the daemon.
