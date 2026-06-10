# Architecture

Repowire is a local-first routing daemon plus thin transport adapters for each agent runtime.

```text
Agent runtime
  ├─ hooks + MCP (Claude Code, Codex, Gemini)
  ├─ plugin + WebSocket (OpenCode)
  ├─ extension (Pi)
  └─ channel / ACP transport (Claude Code experimental)
        ↓
HTTP/WebSocket daemon on 127.0.0.1:8377
        ↓
Dashboard, Telegram, Slack, orchestrator peers, relay, and other peers
```

The daemon is the single routing hub. It does not care whether a peer arrived through hooks, an OpenCode plugin, the Pi extension path, a bot, relay traffic, or experimental channel/ACP delivery. Every peer is represented in the registry and routes messages through the same core message layer.

<p align="center">
  <img src="../assets/repowire-arch.webp" alt="Repowire architecture diagram" width="700" />
</p>

## Core modules

| Area | Files | Responsibility |
| --- | --- | --- |
| Agent backends | `repowire/agent_types.py`, `repowire/agent_backends.py` | Serialized backend identity plus setup, spawn, resume prevalidation, history loading, MCP config, and post-spawn behavior dispatch |
| Configuration | `repowire/config/models.py`, `repowire/config/paths.py`, `repowire/config/spawn.py`, `repowire/config/relay.py` | Main config value object plus narrow path, spawn, and relay config slices for callers that do not need the whole model |
| Daemon app | `repowire/daemon/app.py`, `repowire/daemon/deps.py` | FastAPI app factory, dependency wiring, dashboard/static serving |
| Peer state | `repowire/daemon/peer_registry.py`, `repowire/daemon/registry_identity.py`, `repowire/daemon/registry_events.py`, `repowire/daemon/registry_repair.py` | Registration, identity mapping, liveness, circles, roles, lazy repair, and contradiction events |
| Message routing | `repowire/daemon/peer_delivery.py`, `repowire/daemon/transport_router.py`, `repowire/daemon/message_router.py`, `repowire/daemon/websocket_transport.py` | Delivery orchestration, ACP-before-WebSocket transport selection, and wire delivery over connected peers |
| Ask lifecycle | `repowire/daemon/ask_tracker.py`, `repowire/daemon/routes/asks.py` | Open ask state, pending reminders, close/ack handling |
| Session controls | `repowire/daemon/session_controls.py`, `repowire/daemon/routes/sessions.py` | Session-binding resolution and session-targeted control capability routes |
| Session acquisition | `repowire/daemon/session_control.py`, `repowire/daemon/state/operations.py` | Durable operation records and live-executor acquisition for session/job work |
| Schedules | `repowire/daemon/scheduler.py`, `repowire/daemon/schedule_store.py`, `repowire/daemon/routes/schedules.py` | One-shot and recurring cron deliveries |
| Jobs | `repowire/daemon/job_runner.py`, `repowire/daemon/state/work.py`, `repowire/daemon/state/calendar.py`, `repowire/daemon/routes/work.py` | Durable tracked work, recurring calendar templates, and spawned job dispatch (asks sent from the built-in `@jobs` service peer) |
| Fire completion | `repowire/daemon/job_completion.py`, `repowire/daemon/job_release.py` | Structural job results: arms fires from the dispatch prompt, records the executor's final turn as the result, fails fires on executor death, emits `job_state_changed` events, notifies owners, releases executors |
| Hooks | `repowire/hooks/` | Runtime event adapters, tmux injection, transcript/chat extraction |
| MCP server | `repowire/mcp/server.py` | Agent-facing tools over stdio |
| Control surfaces | `web/`, `repowire/telegram/bot.py`, `repowire/slack/bot.py` | Dashboard and human/service peers |
| Relay | `repowire/relay/server.py`, `repowire/daemon/relay_client.py` | Hosted remote dashboard and cross-machine tunnel |

## Transports

### Hooks + MCP

Claude Code, Codex, and Gemini use lifecycle hooks for registration/status/chat extraction and MCP tools for outbound commands. The hook adapter normalizes each runtime's event names and response fields.

Default message delivery still uses tmux injection plus Stop-hook reminders for unacked asks. The MCP server lazily registers on tool calls so runtimes that initialize late, especially Codex, still get a peer identity before routing.

If a hook-backed orchestrator reconnects after daemon restart or WebSocket churn without carrying its prior `peer_id`, the daemon may reclaim the existing offline identity when the role, display name, circle, backend, and path match unambiguously. Queued notifications for that peer are replayed over the renewed WebSocket before falling back to Stop-hook or CLI draining.

### OpenCode plugin and Pi extension

OpenCode does not expose the same hook shape, so Repowire installs a TypeScript plugin. The plugin holds a WebSocket connection to the daemon and bridges OpenCode session events into the same peer/message model. Pi uses Repowire's extension path when setup detects the `pi` CLI or config.

### Channel / ACP transport

`repowire setup --experimental-channels` installs Claude Code's experimental channel/ACP transport. Messages arrive as `<channel source="repowire">` tags. The normal `repowire mcp` server remains installed for stable tools such as `ask`, `ack`, `notify_peer`, schedules, and peer listing. This requires Claude Code support, claude.ai login, and `bun`. Treat this path as experimental; hooks + MCP remain the default.

### Relay

The daemon connects outbound to the hosted relay over WSS. The relay tunnels dashboard HTTP/SSE calls and bridges WebSocket traffic without requiring inbound access to the user's machine.

## Lazy repair

Repowire avoids polling loops. Liveness repair, persistence flushes, and ghost cleanup are piggy-backed on user-visible requests, bounded by cooldowns. The design rule is: repair when needed, not on timers.

When lazy repair detects a self-inconsistent peer (e.g. online but no live WebSocket, a missing pane, or a dead agent pid) it emits a `peer_contradiction` event once per transition, so silent failures surface in the dashboard stream. `repowire peer doctor <peer>` is the explicit, operator-triggered counterpart: it runs the same reconciliation, then reports identity, inbound reachability, pending asks, and contradictions on demand. `repowire peer rehook <peer>` is the non-destructive recovery (re-establish the inbound ws-hook without killing the pane). A delivery trace ledger (`repowire trace <id>`) records per-message ask/notify stages for post-hoc "where did it go" inspection, persisted in a dedicated SQLite table rather than the bounded dashboard event buffer.

## v0.14 session-native direction

The current stable surface is peer-oriented, but the v0.14 architecture train is moving toward a session-first mesh:

- Sessions become the durable unit of work.
- Peers remain runtime executors.
- Ask/notify delivery now goes through a delivery service plus transport router; WebSocket hooks, experimental ACP, relay, and future transports continue moving toward transport-neutral routing.
- The dashboard currently shows a selected peer/session timeline, merging Claude transcript history where available with realtime events.
- The first session-targeted control routes resolve `repowire_session_id` bindings to an active executor or explicit resume capability status.
- Broader composer actions, scheduling, approval handling, and backend/model controls move toward the same shared session command surface.

This is a roadmap. Current routes and tools still expose peers, circles, asks, notifications, and schedules. The ask/notify delivery-service and transport-router extraction has landed, but ACP remains experimental and not every route/control path is transport-neutral yet.

## Knowledge graph

`graphify-out/GRAPH_REPORT.md` summarizes the codebase graph. The current report identifies the main hubs as `AgentType`, `Config`, `PeerRegistry`, `MessageRouter`, and `WebSocketTransport`, with communities around daemon routing, CLI/setup, channel installer, Telegram, attachments, hook normalization, relay auth, and peer lifecycle.

Keep generated graph JSON and cache files out of prose docs. Link or summarize the report when useful; do not paste large graph artifacts into README or hand-written docs.
