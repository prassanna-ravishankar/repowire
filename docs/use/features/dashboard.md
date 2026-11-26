# Dashboard

## What it is

The dashboard is a browser control surface served by the daemon at `http://localhost:8377/dashboard` or remotely through `https://repowire.io/dashboard`. It is an HTTP and server-sent-events client of the daemon, not a mesh peer: it reads and acts through the same routes agents and bots use, and renders the human inbox as a surface over its event stream (`/events/stream?surface=dashboard`).

## When to use it

Use the dashboard when you want a visual peer roster, live mesh log, per-peer session timeline, your inbox, Jobs view, attachments, spawn controls, or session controls without staying inside one agent terminal.

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
7. Read and answer what agents sent you in the **Inbox** view.

## Inbox

The dashboard is a surface of your [human inbox](../../concepts/human-inbox.md). The **Inbox** tab lists everything agents sent you, newest first, with the sender and kind (message, ask, reply, broadcast); the nav shows the unread count. Items arrive live while the dashboard is open.

- Opening an item marks it read on every surface, Telegram included.
- The reply box under an item answers it, exactly as `repowire inbox reply` does: on an open ask it closes the ask with your text; on anything else it sends the sender a new ask. A choice question also shows its options.
- Reads and replies made on Telegram or the CLI show up here as they happen, and the inbox is reread whenever the dashboard reconnects to the daemon.

A peer's **Messages** tab lists the messages it received (notifies, asks, replies and broadcasts, newest 50, with an unread-only filter) and its delivery mode (`push` or `pull`). Opening one shows its body without marking it read, so looking from the dashboard never changes what the peer sees as new. It reads `GET /messages?peer=` and `GET /messages/{id}` ([HTTP API](../../reference/http-api.md)).

**Broadcast** (the megaphone in the top bar) sends one message to every online agent in every circle. It is mesh-wide, so it is not part of a peer's composer: the dialog lists the agents on this machine it will reach and asks for that count before sending. Agents on your other machines in a shared cross-machine circle are reached too; the dialog says so, and the result names them (sent, queued, failed, or unconfirmed) beside the local ones the daemon reached. It uses `POST /broadcast` from the dashboard.

**Scheduling.** Under the composer's ask/notify switch, **now / later / repeat** chooses when the send goes out. *Later* takes a date and time in your local time. *Repeat* takes a five-field cron expression, which the daemon evaluates in **UTC**; presets cover the common cases. The ask/notify switch is the schedule's kind. A scheduled send carries text only, so attachments cannot be scheduled. A scheduled notify to an offline peer is queued like any notify, but a scheduled ask is not: if the peer is offline when it fires, it fails, and the scheduler tells you in the inbox. The peer's pending schedules made from the dashboard are listed under the composer with a cancel button (schedules made from the CLI, Telegram or MCP are listed by `repowire schedule list`), and the composer returns to *now* after each scheduled send. It uses `POST /schedules`, `GET /schedules` and `DELETE /schedules/{id}`.

The peer chat view merges supported persisted local history with realtime `chat_turn` and `chat_turn_delta` events for Claude Code, Codex, and Codex ACP peers. Backends without a supported local history source contribute realtime events and report degraded history state in the timeline response.

## Commands and API

The dashboard uses daemon HTTP and SSE routes directly:

- `GET /peers` and event streams for roster and mesh log. The dashboard opens `GET /events/stream?surface=dashboard`, which makes it a live inbox surface.
- `GET /inbox`, `POST /inbox/{id}/read`, and `POST /inbox/{id}/reply` (with `surface: "dashboard"`) for the inbox.
- Peer timeline, transcript, and search routes for chat history.
- `POST /ask` and `POST /notify` for compose sends.
- `POST /attachments` for uploads.
- `POST /sessions/{repowire_session_id}/controls/notify` for nudging an active executor attached to a captured session.
- `POST /sessions/{repowire_session_id}/controls/resume` for backend-native resume when a detached session binding is resumable.

Dashboard spawn and backend controls use the same allowed paths, backend commands, and profile configuration as CLI and MCP surfaces.

The peer header's `Fork to…` control starts a sibling agent on another configured backend in the same project and circle. The source peer stays alive, so the control is available for pane-less native sessions as well as Repowire-managed tmux peers. The fork shares workspace state but does not clone backend-native conversation history. Success and rejected forks are shown inline in the peer header.

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

## Command palette and peer description

Press **Cmd-K** (macOS) or **Ctrl-K** (elsewhere; on macOS Ctrl-K stays the text fields' delete-to-end-of-line) to open the command palette, from anywhere but another open dialog. It lists the dashboard's own actions: broadcast, spawn, go to inbox, jobs or the mesh feed, settings and refresh. It also lists every visible peer to open, and, with a peer open, **Set description**. Type any words of a command or a peer's name, circle or status to filter; use the arrow keys and Enter to run one, and Escape to close. The palette adds no action of its own: each command does what its button does.

A peer's description (its current task, as an agent sets it with `set_description`) shows under its name. Edit it with the pencil, or from the palette. Enter saves it through `POST /peers/{id}/description`, an empty description clears it, and Escape cancels. It is the same field the agent sets, so the agent's next `set_description` replaces what you wrote.
