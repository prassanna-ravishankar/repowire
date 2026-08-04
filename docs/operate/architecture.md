# Architecture

Repowire is a local-first routing daemon plus thin transport adapters for each agent runtime.

```text
Agent runtime
  ├─ Go hooks + stdio identity shim (Claude Code, Gemini)
  ├─ App Server bridge + MCP (Codex)
  ├─ plugin + WebSocket (OpenCode)
  ├─ extension (Pi)
  └─ Channel bridge / ACP transport (Claude Code experimental)
        ↓
Go HTTP/WebSocket/MCP daemon on 127.0.0.1:8377
        ↓
Dashboard, Telegram, Slack, orchestrator peers, relay, and other peers
```

The daemon is the single routing hub. It does not care whether a peer arrived through hooks, an OpenCode plugin, the Pi extension path, a bot, relay traffic, or experimental channel/ACP delivery. Every peer is represented in the registry and routes messages through the same core message layer.

Runtime-side components that translate a native session API into this common
protocol are called [bridges](../concepts/bridges.md). Their lifecycle follows
the runtime: Codex needs a supervised companion, while Claude Channels,
OpenCode, and Pi load their bridge inside the agent session.

<p align="center">
  <img src="../assets/repowire-arch.webp" alt="Repowire architecture diagram" width="700" />
</p>

## Core modules

| Area | Files | Responsibility |
| --- | --- | --- |
| CLI and installers | `daemon-go/cli/` | Setup, runtime installation, service/peer/job/schedule/session commands, embedded client assets |
| Configuration | `daemon-go/config/` | YAML/default/environment loading for daemon, spawn, relay, MCP, and experiments |
| Daemon routes | `daemon-go/hub/` | HTTP/WebSocket server, dashboard, history/timeline, daemon endpoints, and `/mcp` |
| Peer state | `daemon-go/peer/` | Typed identity registry, lifecycle FSM, circles, roles, lazy repair, and contradictions |
| Message routing | `daemon-go/service/delivery.go`, `daemon-go/service/router.go`, `daemon-go/service/transport.go`, `daemon-go/service/acp.go` | ACP-before-WebSocket delivery, receipts, queuing, and transport lifecycle |
| Ask lifecycle | `daemon-go/service/ask_tracker.go`, `daemon-go/hub/routes_ask_lifecycle.go` | Open asks, reminders, structured answers, and ack/reply delivery |
| Sessions and spawn | `daemon-go/service/session_control.go`, `daemon-go/service/spawn_service.go` | Resume prevalidation, destructive pane proof, executor acquisition, and controls |
| Schedules and jobs | `daemon-go/service/scheduler.go`, `daemon-go/service/job_runner.go`, `daemon-go/service/job_completion.go`, `daemon-go/state/` | Deadline-driven schedules, durable work, turn completion, executor-death failure, SQLite state |
| Hooks | `daemon-go/hooks/` | Runtime adapters, ws-hook supervision, tmux injection, chat extraction, remote approval |
| Codex bridge | `daemon-go/codexbridge/` | App Server lifecycle, native thread steering, and chat events |
| MCP | `daemon-go/hub/routes_mcp*.go`, `daemon-go/mcpstdio/` | Complete daemon-owned HTTP tool surface plus the per-runtime identity proxy |
| Control surfaces | `web/`, `daemon-go/mobile/` | Dashboard and native Telegram/Slack human peers |
| Relay | `daemon-go/relayserver/`, `daemon-go/relay/` | Native hosted server plus the daemon's outbound tunnel client |

## Transports

### Hooks + MCP

Claude Code and Gemini use native Go lifecycle hooks for registration,
status, and chat extraction. Their `repowire mcp` process resolves a
daemon-minted runtime certificate and proxies tool JSON-RPC to `/mcp`, where all
31 tool implementations live.

Default message delivery still uses tmux injection plus Stop-hook reminders for
unacked asks. The identity shim lazily registers on tool calls so runtimes that
initialize late, especially Codex, still get a peer identity. The daemon ignores
a claimed identity header unless its certificate proof is current and bound to
that peer.

### Codex App Server

The separately supervised Codex companion owns the default Unix control socket
and translates App Server threads into ordinary mesh peers. `thread/started`
registers a peer before its first prompt; turn and item notifications drive
status and dashboard chat events. Inbound delivery uses `turn/steer` for an
active turn or `turn/start` for an idle thread. Restarting the routing daemon
does not restart App Server or the Codex TUI.

If a hook-backed orchestrator reconnects after daemon restart or WebSocket churn without carrying its prior `peer_id`, the daemon may reclaim the existing offline identity when the role, display name, circle, backend, and path match unambiguously. Queued notifications for that peer are replayed over the renewed WebSocket before falling back to Stop-hook or CLI draining.

### OpenCode plugin and Pi extension

OpenCode does not expose the same hook shape, so Repowire installs a TypeScript plugin. The plugin holds a WebSocket connection to the daemon and bridges OpenCode session events into the same peer/message model. Pi uses Repowire's extension path when setup detects the `pi` CLI or config.

### Claude Channel bridge / ACP transport

`repowire setup --experimental-channels` installs the embedded TypeScript Claude
Channel bridge. Messages arrive as `<channel source="repowire">` tags. The
HTTP-backed MCP identity shim remains installed for stable tools. The Go daemon
also contains the experiment-gated ACP subprocess client and maps ACP permission
requests onto the shared blocking-question path. Channel mode still requires
Claude Code support, claude.ai login, and `bun`.

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
- The dashboard currently shows a selected peer/session timeline, merging Claude Code or Codex local history where available with realtime events.
- The first session-targeted control routes resolve `repowire_session_id` bindings to an active executor or explicit resume capability status.
- Broader composer actions, scheduling, approval handling, and backend/model controls move toward the same shared session command surface.

This is a roadmap. Current routes and tools still expose peers, circles, asks, notifications, and schedules. The ask/notify delivery-service and transport-router extraction has landed, but ACP remains experimental and not every route/control path is transport-neutral yet.

## Knowledge graph

`graphify-out/GRAPH_REPORT.md` summarizes the codebase graph. The current report identifies the main hubs as `AgentType`, `Config`, `PeerRegistry`, `MessageRouter`, and `WebSocketTransport`, with communities around daemon routing, CLI/setup, channel installer, Telegram, attachments, hook normalization, relay auth, and peer lifecycle.

Keep generated graph JSON and cache files out of prose docs. Link or summarize the report when useful; do not paste large graph artifacts into README or hand-written docs.
