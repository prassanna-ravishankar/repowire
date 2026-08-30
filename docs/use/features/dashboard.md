# Dashboard

## What it is

The dashboard is a browser control surface served by the daemon at `http://localhost:8377/dashboard` or remotely through `https://repowire.io/dashboard`. It registers as the `dashboard` peer and uses the same mesh routes as agents and bots.

## When to use it

Use the dashboard when you want a visual peer roster, live mesh log, per-peer session timeline, Jobs view, attachments, spawn controls, or session controls without staying inside one agent terminal.

Use Telegram or Slack when you primarily want phone or team-chat access. Use CLI/MCP when you need scriptable commands.

## Setup

Start the daemon:

```bash
repowire serve
```

Open:

```text
http://localhost:8377/dashboard
```

For remote access without inbound ports, enable [Relay access](relay-access.md) and open `https://repowire.io/dashboard`.

## Common workflows

1. Confirm expected peers are online in the peer list.
2. Select a peer to inspect its selected-session timeline.
3. Send a tracked ask or fire-and-forget notify from the compose bar.
4. Attach files or images when the target agent needs local file context.
5. Inspect the live mesh log for asks, acks, notifications, broadcasts, and structured questions.
6. Use the Jobs view to inspect, retry, run, or cancel eligible durable work.

The peer chat view merges supported persisted local history with realtime `chat_turn` and `chat_turn_delta` events for Claude Code, Codex, and Codex ACP peers. Backends without a supported local history source contribute realtime events and report degraded history state in the timeline response.

## Commands and API

The dashboard uses daemon HTTP and SSE routes directly:

- `GET /peers` and event streams for roster and mesh log.
- Peer timeline, transcript, and search routes for chat history.
- `POST /ask` and `POST /notify` for compose sends.
- `POST /attachments` for uploads.
- `POST /sessions/{repowire_session_id}/controls/notify` for nudging an active executor attached to a captured session.
- `POST /sessions/{repowire_session_id}/controls/resume` for backend-native resume when a detached session binding is resumable.

Dashboard spawn and backend controls use the same allowed paths, backend commands, and profile configuration as CLI and MCP surfaces.

Backend switching appears only for Repowire-managed tmux peers with a recorded pane id. Pane-less native sessions, including Codex App Server threads, show `native · no switch`: Repowire cannot prove and terminate their exact runtime process safely, so it does not offer a destructive control. A rejected switch on an eligible peer is shown inline in the peer header.

## Limits

- The dashboard does not poll. It streams daemon events and receives chat turns from runtime hooks or plugin bridges.
- OpenCode and Pi expose realtime events but do not expose a supported local history source in this slice.
- Session actions require a captured durable `repowire_session_id`; legacy or partial bindings remain visible in the timeline but do not show live controls.
- The dashboard already sees turns automatically, so agents rarely need to explicitly notify `dashboard`.

## Troubleshooting

- Dashboard cannot connect: check [Daemon unreachable](../../troubleshooting/daemon.md).
- Expected peers are missing: run `repowire peer list` and compare the daemon view.
- Chat history is degraded: confirm the backend supports persisted local history and hooks are firing.
- Remote dashboard is offline: check [Relay key rotation](../../troubleshooting/relay-keys.md) and [Relay access](relay-access.md).

## See also

- [Control surfaces](../../concepts/control-surfaces.md)
- [Attachments](attachments.md)
- [Jobs](jobs.md)
- [Spawning](spawning.md)
- [Relay access](relay-access.md)
- [HTTP API](../../reference/http-api.md)
