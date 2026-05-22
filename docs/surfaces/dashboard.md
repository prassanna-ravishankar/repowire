# Web dashboard

The dashboard is a Next.js UI served by the daemon at `http://localhost:8377/dashboard`, or remotely via the relay at `https://repowire.io/dashboard`.

## What it shows

- **Peer overview** — every peer's status (`online` / `busy` / `offline`), description, project path, circle, backend.
- **Live mesh log** — `mesh.log`, a chronological event stream of asks, acks, notifications, and broadcasts.
- **Per-peer chat** — selecting a peer replaces the live log with that peer's selected-session timeline. For Claude Code, Codex, and Codex ACP peers, the chat view merges supported persisted local history with realtime `chat_turn` and `chat_turn_delta` events; backends without a supported local history source contribute realtime events and report that degraded state in the timeline response. User and `@dashboard` messages align right; peer messages align left. Tool calls collapse behind a disclosure.
- **Compose bar** — send a plain-text ask to any peer. The dashboard registers as the `dashboard` peer so it shows up in `list_peers` and message routing. This remains the default path for conversational requests and attachments.
- **Session controls** — when the selected chat has a Repowire session binding, a compact command surface can inspect resume capability and send fire-and-forget notifications to the active executor with `POST /sessions/{repowire_session_id}/controls/notify`. Sessions without an active executor, without a selected binding, or with unsupported resume metadata show disabled controls with the daemon-provided capability message.
- **Spawn and backend controls** — spawn a peer or switch a peer backend using the runtime profiles from `daemon.spawn.commands` and the roots from `daemon.spawn.allowed_paths`.
- **MCP config tab** — supported peers can list, add, and remove MCP servers. The tab labels whether edits affect peer/project config or backend user-global config before showing edit controls.
- **Attachments** — the compose bar has a file upload button. Files post to `POST /attachments` (10 MB limit, 24 h TTL). The outgoing ask carries structured attachment metadata plus a text fallback with the local path so existing agents can still read it.
- **Attachment chips** — mesh events and per-peer chat render attachment chips with download links when an attachment ID is available.

## Where chat turns come from

The dashboard does not poll. The stop hook of every Claude Code, Codex, and Gemini session extracts the response and tool calls from the transcript or runtime output, then posts them to `POST /events/chat` on the daemon. Claude Code can also stream block-level `chat_turn_delta` events while a turn is in progress. The dashboard streams events over Server-Sent Events and merges them with supported backend-local history for the selected peer/session. OpenCode and Pi bridge the same realtime shape via plugins/extensions, but do not expose a supported local history source in this v0.13 slice.

ACP-routed permission prompts emit `acp_permission_request` and `acp_permission_decision` events into the same daemon event stream. Human control surfaces can resolve a pending prompt with `POST /acp/permissions/{request_id}/decision`; if no decision arrives before the broker timeout, the daemon records a timed-out decision and denies by default. This v0.13 slice exposes the event/route contract only; the dashboard approval UI remains planned work.

## Session command direction

The v0.13 dashboard direction is a timeline-centered view. The current slice merges supported persisted history and realtime stream events for the selected peer/session. Peers remain the runtime executors, and broader controls such as model/backend switching, resume, scheduling, approval handling, and plan-mode decisions remain roadmap items that should attach to shared session commands as those features land.

`GET /peers/{name}/timeline` is the first daemon-side session timeline read model. It returns normalized items with explicit `session_id`, `turn_id`, `source` (`history` or `realtime`), and `kind` (`turn` or `delta_group`) by merging supported local history turns with the daemon's buffered realtime `chat_turn` and `chat_turn_delta` events. The response also includes `history_status`, `history_backend`, and `history_message` so callers can distinguish loaded history from unsupported or unavailable backend history. When a session binding is available, history lookup resolves through that binding's runtime session/source locator and reports `history_source`, `repowire_session_id`, `binding_status`, and `runtime_session_id`; otherwise it falls back to the peer/path discovery used by earlier v0.13 slices. Claude Code history is loaded from project transcripts, Codex and Codex ACP history is loaded from rollout JSONLs matched by runtime cwd or binding source locator, and Gemini/OpenCode/Pi currently report unsupported local history while retaining realtime timeline events. This route is additive; existing `/events` and dashboard SSE behavior are unchanged.

`GET /peers/{name}/transcript` uses the same binding-backed history resolution and fallback behavior, while preserving its existing paginated transcript fields.

`POST /sessions/{repowire_session_id}/controls/resume` is used by the dashboard as a dry-run capability probe. It reports whether the selected binding already has an active executor, has backend resume metadata available for future callers, or is unsupported/unavailable. In this v0.13 slice the dashboard only executes active-executor notify controls; backend-specific resume execution, scheduling controls, approval handling, and plan-mode decisions remain roadmap items for the shared session command surface.

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
