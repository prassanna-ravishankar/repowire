# Web dashboard

The dashboard is a Next.js UI served by the daemon at `http://localhost:8377/dashboard`, or remotely via the relay at `https://repowire.io/dashboard`.

## What it shows

- **Peer overview** — every peer's status (`online` / `busy` / `offline`), description, project path, circle, backend.
- **Live mesh log** — `mesh.log`, a chronological event stream of asks, acks, notifications, broadcasts, and structured questions.
- **Jobs view** — inspect durable work and recurring job templates from the daemon's `/jobs` surface, then run, retry, or cancel eligible jobs without leaving the dashboard.
- **Per-peer chat** — selecting a peer replaces the live log with that peer's selected-session timeline. For Claude Code, Codex, and Codex ACP peers, the chat view merges supported persisted local history with realtime `chat_turn` and `chat_turn_delta` events; backends without a supported local history source contribute realtime events and report that degraded state in the timeline response. User and `@dashboard` messages align right; peer messages align left. Tool calls collapse behind a disclosure.
- **Conversation search** — the peer chat view can search the normalized timeline and jump to a matching turn. Results carry the session/turn cursor used by the timeline, so jumping can switch to the matched session before scrolling. If only the daemon's realtime event ring is searchable because persisted local history is unavailable or unsupported, the dashboard shows that degraded state beside the results.
- **Compose bar** — send a plain-text ask to any peer. The dashboard registers as the `dashboard` peer so it shows up in `list_peers` and message routing. This remains the default path for conversational requests and attachments.
- **Session actions** — when the selected captured session has a running agent attached, the dashboard can send fire-and-forget nudges such as status, checkpoint, and continue through `POST /sessions/{repowire_session_id}/controls/notify`. When no agent is running but the binding has a backend runtime session id, the dashboard can start a backend-native resume through `POST /sessions/{repowire_session_id}/controls/resume`. Legacy or partial bindings stay visible in the timeline but show a disabled control state.
- **Spawn and backend controls** — spawn a peer or switch a peer backend using the runtime profiles from `daemon.spawn.commands`, optional model profiles from `daemon.spawn.profiles`, and the roots from `daemon.spawn.allowed_paths`.
- **MCP config tab** — supported peers can list, add, and remove MCP servers. The tab labels whether edits affect peer/project config or backend user-global config before showing edit controls.
- **Attachments** — the compose bar has a file upload button. Files post to `POST /attachments` (10 MB limit, 24 h TTL). The outgoing ask carries structured attachment metadata plus a text fallback with the local path so existing agents can still read it.
- **Attachment chips** — mesh events and per-peer chat render attachment chips with download links when an attachment ID is available.
- **Command history** — from an empty composer, ArrowUp recalls previously sent asks for the selected peer (ArrowDown walks forward, Escape returns to empty). History is per-peer and in-memory for the session; once you start editing a recalled entry, arrows behave normally so multi-line editing is unaffected. The plain-text ask flow and Cmd/Ctrl+Enter submit are unchanged.
- **Pending questions** — structured questions, including ACP tool-permission prompts, appear as an answer banner above the dashboard grid. Choice questions render option buttons; tool-permission questions also render an explicit Deny action. Answers post to the shared `/answer` route and close when the daemon emits the matching ack event.

## Where chat turns come from

The dashboard does not poll. The stop hook of every Claude Code, Codex, and Gemini session extracts the response and tool calls from the transcript or runtime output, then posts them to `POST /events/chat` on the daemon. Claude Code can also stream block-level `chat_turn_delta` events while a turn is in progress. The dashboard streams events over Server-Sent Events and merges them with supported backend-local history for the selected peer/session. OpenCode and Pi bridge the same realtime shape via plugins/extensions, but do not expose a supported local history source in this v0.14 slice.

ACP-routed permission prompts are represented as structured mesh questions. The daemon emits the normal ask/ack question events for dashboard and Telegram rendering, and also keeps `acp_permission_request` / `acp_permission_decision` events as compatibility/audit aliases. Human control surfaces answer through `POST /answer`; the legacy `POST /acp/permissions/{request_id}/decision` route remains as a compatibility shim. If no decision arrives before the broker timeout, the daemon records a timed-out answer and denies by default.

## Session command direction

The v0.14 dashboard direction is a timeline-centered view. The current slice merges supported persisted history and realtime stream events for the selected peer/session. Peers remain the runtime executors, and broader controls such as model/backend switching, resume, scheduling, and plan-mode decisions remain roadmap items that should attach to shared session commands as those features land.

The daemon route details for timeline, transcript, search, and session-control probes live in [HTTP API](../reference/http-api.md) and [Operations: architecture](../operations/architecture.md). This capability page stays focused on what the dashboard exposes to users.

## Mobile

The dashboard is mobile-responsive: hamburger menu for the peer list, touch-friendly compose bar, sticky bottom switcher between peer roster, mesh log, and jobs. You don't need the Telegram or Slack bot to drive the mesh from a phone — though those surfaces are often more convenient than the mobile dashboard.

## Build and deploy

The dashboard ships pre-built in the `repowire` package. After editing files under `web/`:

```bash
repowire build-ui
```

This produces a static export served by the daemon directly. The relay serves the same static export from its container; only API calls and SSE streams tunnel back to the originating daemon.

## See also

- [Concepts: control surfaces](../concepts/control-surfaces.md) for the "surfaces are peers" framing.
- [Session-native roadmap](../concepts/session-native-roadmap.md) for the dashboard/session direction.
- [Attachments](attachments.md) for upload behavior and limits.
- [Jobs](jobs.md) for durable work behavior and limits.
- [Spawning](spawning.md) for backend/profile-controlled peer launch.
- [Relay access](relay-access.md) for the remote-dashboard tunneling model.
