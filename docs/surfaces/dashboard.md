# Web dashboard

The dashboard is a Next.js UI served by the daemon at `http://localhost:8377/dashboard`, or remotely via the relay at `https://repowire.io/dashboard`.

## What it shows

- **Peer overview** — every peer's status (`online` / `busy` / `offline`), description, project path, circle, backend.
- **Live mesh log** — `mesh.log`, a chronological event stream of asks, acks, notifications, and broadcasts.
- **Per-peer chat** — selecting a peer replaces the live log with that peer's selected-session timeline. For Claude Code peers, the chat view merges persisted transcript history with realtime `chat_turn` and `chat_turn_delta` events; other backends contribute realtime events. User and `@dashboard` messages align right; peer messages align left. Tool calls collapse behind a disclosure.
- **Compose bar** — send a notification or ask to any peer. The dashboard registers as the `dashboard` peer so it shows up in `list_peers` and message routing.
- **MCP config tab** — supported peers can list, add, and remove MCP servers. The tab labels whether edits affect peer/project config or backend user-global config before showing edit controls.
- **Attachments** — the compose bar has a file upload button. Files post to `POST /attachments` (10 MB limit, 24 h TTL). The outgoing ask carries structured attachment metadata plus a text fallback with the local path so existing agents can still read it.
- **Attachment chips** — mesh events and per-peer chat render attachment chips with download links when an attachment ID is available.

## Where chat turns come from

The dashboard does not poll. The stop hook of every Claude Code, Codex, and Gemini session extracts the response and tool calls from the transcript or runtime output, then posts them to `POST /events/chat` on the daemon. Claude Code can also stream block-level `chat_turn_delta` events while a turn is in progress. The dashboard streams events over Server-Sent Events and merges them with Claude transcript history for the selected peer/session. OpenCode bridges the same realtime shape via its plugin.

## Roadmap: session timeline

The v0.13 dashboard direction is a timeline-centered view. The current slice merges Claude transcript history and realtime stream events for the selected peer/session. Peers remain the runtime executors, and broader controls such as model/backend switching, resume, scheduling, approval handling, and plan-mode decisions remain roadmap items that should attach to shared session commands as those features land.

`GET /peers/{name}/timeline` is the first daemon-side session timeline read model. It returns normalized items with explicit `session_id`, `turn_id`, `source` (`history` or `realtime`), and `kind` (`turn` or `delta_group`) by merging Claude transcript turns with the daemon's buffered realtime `chat_turn` and `chat_turn_delta` events. This route is additive; existing `/peers/{name}/transcript`, `/events`, and dashboard SSE behavior are unchanged.

## Mobile

The dashboard is mobile-responsive: hamburger menu for the peer list, touch-friendly compose bar, sticky bottom switcher between peer roster and mesh log. You don't need the Telegram or Slack bot to drive the mesh from a phone — though those surfaces are often more convenient than the mobile dashboard.

## Build and deploy

The dashboard ships pre-built in the `repowire` package. After editing files under `web/`:

```bash
repowire build-ui
```

This produces a static export served by the daemon directly. The relay serves the same static export from its container; only API calls and SSE streams tunnel back to the originating daemon.

## See also

- [Concepts: control surfaces](../concepts/control-surfaces.md) for the "surfaces are peers" framing.
- [Session-native roadmap](../concepts/session-native-roadmap.md) for the dashboard/session direction.
- [Hosted relay](../relay/hosted.md) for the remote-dashboard tunneling model.
