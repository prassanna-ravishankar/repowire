# Repowire Graphify Architecture Report

Generated: 2026-05-19  
Worktree: `/Users/prass/development/projects/repowire.graphify`  
Branch: `docs/graphify-repowire`

## Scope and Method

Requested practical scope was README/agent instructions, docs, daemon, MCP, dashboard, and docs app surfaces. I used the graphify skill workflow as far as this runtime permits:

```bash
sed -n '1,760p' /Users/prass/.agents/skills/graphify/SKILL.md
which graphify
bd prime
bd dolt pull
bd ready
bd create --title="Generate graphify architecture report" --description="Analyze scoped repowire docs and architecture files with graphify, produce graphify-out artifacts, and report whether outputs belong in repo or generated artifacts." --type=task --priority=2
graphify update .
```

Important blockers/limitations:

- `bd dolt pull` failed with `Error 1105: no remote`; `bd ready` reported no open issues.
- `bd create` failed because `.beads` is missing `issue_prefix`; I did not initialize or modify Beads metadata.
- Full graphify semantic extraction in the skill requires parallel extraction subagents. This runtime only permits spawning subagents when explicitly requested, so I did not run LLM semantic extraction subagents.
- `graphify update .` refreshed the deterministic/code graph only. The architecture conclusions below are therefore a hybrid of graphify AST output plus direct inspection of docs and architecture modules.
- A direct cross-circle ask to `repowire.v013-session-arch-codex` was blocked by circle boundary; session-native roadmap context was routed via `orchestrator-2-codex` and is included only as roadmap language.

## Graph Outputs

`graphify update .` completed successfully:

```text
Re-extracting code files in . (no LLM needed)...
  AST extraction: 207/207 files (100%)
[graphify watch] Rebuilt: 4326 nodes, 14541 edges, 224 communities
[graphify watch] graph.json, graph.html and GRAPH_REPORT.md updated in graphify-out
Code graph updated. For doc/paper/image changes run /graphify --update in your AI assistant.
```

Generated/updated files:

- `graphify-out/graph.json` — graphify generated a 7.1 MB snapshot with 4,326 nodes, 14,541 links, and 3 hyperedges during the run. I restored this tracked large artifact after inspection because it should stay generated/out-of-band unless the repo intentionally tracks refreshed snapshots.
- `graphify-out/graph.html` — graphify generated a 4.9 MB interactive graph during the run. I restored this tracked large artifact after inspection for the same reason.
- `graphify-out/GRAPH_REPORT.md` — this focused architecture report.
- `graphify-out/cache/` contains generated cache JSON files; untracked cache files from this run were removed.

Repo suitability:

- `GRAPH_REPORT.md` is suitable to commit as a docs handoff artifact.
- `graph.json`, `graph.html`, `manifest.json`, `cost.json`, and cache files are generated artifacts. They are useful locally, but at their current sizes they should stay generated/out-of-band unless the project intentionally tracks graphify output snapshots.
- Existing tracked graphify artifacts were already present before this run. I did not edit product runtime code.

## Graphify Hub Findings

The deterministic graph identifies these most connected abstractions:

1. `AgentType`
2. `Config`
3. `PeerRegistry`
4. `PeerStatus`
5. `Peer`
6. `MessageRouter`
7. `WebSocketTransport`
8. `QueryTracker`
9. `PeerRole`
10. `SpawnConfig`

Read as architecture signal: Repowire is organized around typed peer identity, daemon configuration, registry lifecycle, and transport-neutral routing through a WebSocket-backed hub. The graph also surfaced three useful cross-cutting hyperedges already present in the output: Telegram reply-keyboard pipeline, hook-to-daemon HTTP utilities, and OpenCode incoming-query lifecycle.

## Architecture Communities

### Mesh and MCP Tools

Primary files: `repowire/mcp/server.py`, `docs/reference/mcp-tools.md`, `docs/concepts/message-types.md`, `docs/concepts/peers-and-circles.md`.

The MCP server is intentionally a thin HTTP client over the local daemon. Every tool entry calls lazy registration/touch logic, resolves the caller identity, and sends HTTP requests to daemon routes. The public tool surface is the mesh API agents actually use:

- Routing: `ask`, `ack`, `notify_peer`, `broadcast`.
- Inspection: `list_peers`, `whoami`, `set_description`, `orchestrator_status`.
- Lifecycle: `spawn_peer`, `kill_peer`.
- Review/scheduling: `mark_reviewed`, `review_queue`, `schedule_create`, `schedule_self`, `schedule_cron`, `schedule_list`, `schedule_delete`.

Key docs point: the distinction between `ask` and `notify_peer` is lifecycle, not message content. `ask` opens a durable correlation that must be closed by `ack`; `notify_peer` is fire-and-forget. `ack` replies are routed back to the original asker independent of current circle because the thread was established at ask time.

Identity is central. The MCP server caches daemon-assigned `peer_id` when available, falls back to display name only when needed, and scopes send/list behavior by caller circle unless targeting role-bypass peers (`orchestrator`, `service`, human surfaces). This is the basis for the README claim that agents can address peers by name while the daemon prevents ambiguous misroutes.

### Daemon Routing and Scheduler

Primary files: `repowire/daemon/app.py`, `repowire/daemon/peer_registry.py`, `repowire/daemon/message_router.py`, `repowire/daemon/ask_tracker.py`, `repowire/daemon/query_tracker.py`, `repowire/daemon/scheduler.py`, `repowire/daemon/schedule_store.py`, `repowire/daemon/routes/*.py`.

The daemon is the single routing hub. `create_app()` wires the component stack on startup:

- `WebSocketTransport` owns live peer connections.
- `QueryTracker` preserves legacy blocking query futures.
- `AskTracker` stores first-class open ask state.
- `MessageRouter` emits `query`, `ask`, `notify`, and `broadcast` frames over the transport.
- `PeerRegistry` is the source of truth for peer identity, circle, status, display-name collision handling, persistent session mappings, and events.
- `ScheduleStore` plus `Scheduler` drive one-shot and cron check-ins.
- `ReviewQueueStore`, relay client, Telegram/Slack services, and ACP manager attach around the same hub.

Routing remains explicit:

- `/ask` resolves sender/recipient, registers the ask, optionally chooses ACP routing, otherwise delivers a `type=ask` WebSocket frame.
- `/ack` closes the tracked ask and frames a reply notification back to the stored asker identity.
- `/asks/pending` lets Stop hooks resurface unacked asks on every turn.
- `/notify` and `/broadcast` skip ask lifecycle state.

Scheduler note for docs: the scheduler has a background task, but it sleeps until the next due schedule or a wake event when schedules change. That is consistent with the lazy-repair philosophy because it is schedule-driven rather than peer polling.

### Transports, ACP, and WebSocket

Primary files: `repowire/daemon/routes/websocket.py`, `repowire/daemon/websocket_transport.py`, `repowire/hooks/*`, `repowire/installers/claude_code.py`, `repowire/installers/opencode.py`, `repowire/channel/server.ts`, `repowire/acp/*`, `docs/agents/*.md`, `docs/troubleshooting/channel-auth.md`.

Transport split:

- Default Claude Code, Codex, and Gemini path uses hooks plus a background WebSocket hook/client. Hooks normalize runtime-specific events into canonical session/prompt/stop behavior.
- OpenCode uses a TypeScript plugin with an in-process persistent WebSocket connection.
- Experimental Claude Code channel transport routes messages through an MCP channel server and leaves the Stop hook for dashboard chat turn extraction.
- Telegram, Slack, dashboard, relay, and external clients are control surfaces around the daemon API; they are not separate routing authorities.

The WebSocket protocol is small: clients connect with identity fields, then exchange `response`, `status`, `set_circle`, and related lifecycle messages. Daemon-to-client frames include `query`, `ask`, `notify`, and `broadcast`.

ACP is present but should be documented as experimental. The ACP broker path is gated by `experiments.acp_broker_client` and only applies when the target peer has a valid `metadata["acp"]` block. In that case `/ask` bypasses the WebSocket transport for that peer, prompts a broker-side ACP subprocess, and completes the original ask by notifying the asker. Existing comments call this Phase 3 and keep `notify`, `broadcast`, and non-ACP peers on the WebSocket path.

Suggested public wording: ACP is an experiment toward session-native execution, not production-ready transport replacement.

### Dashboard and Docs

Primary files: `web/app/dashboard/page.tsx`, `web/app/dashboard/components/*`, `web/app/dashboard/lib/useEventStream.ts`, `web/app/docs/*`, `docs/surfaces/dashboard.md`, `docs/design-system.md`.

The dashboard is a live control surface over daemon state:

- It fetches `/peers` and `/events` initially.
- It subscribes to server-sent events via `useEventStream` and keeps a 500-event client-side view.
- It renders peer roster, mesh feed, per-peer thread, MCP panel, history panel, spawn/settings dialogs, mobile tabs, and orchestrator-offline warnings.
- Per-peer chat groups chat turns, ask/notify/response events, streaming deltas, tool calls, attachments, and pending ask state.

The docs app is intentionally smaller than the MkDocs corpus. `web/app/docs/_nav.ts` currently exposes quickstart, concepts, MCP tools, Python client, and CLI. The full `docs/` tree has richer reference and troubleshooting content, including architecture stubs. README/docs refresh should avoid duplicating everything in the README; use README for a concise model and link to docs pages for details.

### Orchestrator Workflows

Primary files: `docs/concepts/orchestrator.md`, `docs/patterns/orchestrator-coordination.md`, `repowire/orchestrator/template/*`, `repowire/mcp/server.py`, scheduler/review routes.

An orchestrator is metadata plus workflow, not a privileged daemon actor. The mesh treats it as a peer with `role=orchestrator`; humans and agents choose to route work through it. Useful workflow primitives:

- `list_peers` defaults to mesh-wide for orchestrator-role callers.
- `orchestrator_status` checks live orchestrator presence per circle.
- `set_description` keeps dashboard state useful during delegated work.
- `review_queue` and `mark_reviewed` connect PR review obligations to peers.
- `schedule_*` tools enable future self/peer check-ins.

Docs should describe the orchestrator as the coordination loop for multi-repo work: scan queue, dispatch to project peer, receive updates, review, release. It should not be framed as access control or a separate scheduler service.

### Session-Native Roadmap

Source: roadmap context routed from `repowire.v013-session-arch-codex` via `orchestrator-2-codex` on 2026-05-19, plus local ACP/config/daemon evidence.

Roadmap-only language for README/docs:

- The v0.13 train is moving from transient peer connection toward durable session.
- Public-safe terms: session-first/session-native, durable session, runtime executor, transport-neutral routing, session timeline, compatible v0.13.x slices.
- Planned sequence: transport router for ask/notify, session/timeline store, dashboard merged persisted plus realtime timeline, session command/composer surface, runtime manager plus approval events.
- Position ACP as one experimental route toward broker-side/runtime execution, not as fully production-ready.

Avoid these claims in public docs for now:

- Do not say session-native is fully shipped.
- Do not label it v0.14.
- Do not claim all history is unified.
- Do not claim model switching or plan approval is implemented.
- Do not say ACP is production-ready.
- Do not claim routes are fully transport-unaware yet.

## README/Docs Implications

High-signal README architecture language:

- Repowire is a local-first mesh for AI coding sessions.
- The daemon is the single routing hub; peers connect through transport adapters.
- All peers speak the same message model: `ask`, `ack`, `notify_peer`, `broadcast`.
- MCP tools are the stable agent-facing API; daemon routes and transports are implementation details.
- Circles are logical subnets for scoping, not authorization.
- Human control surfaces (`dashboard`, `telegram`, `slack`) are peers with human-role routing behavior.
- Lazy repair means state is eventually consistent and repaired on interaction; avoid promising continuous monitoring.
- Experimental surfaces should be named explicitly: channel transport, ACP broker client, chat turn streaming.

Recommended CLAUDE/AGENTS updates:

- Update test count if README/CI has changed since the current 222/231 mismatch.
- Add `repowire/acp/*`, scheduler files, review queue store, and dashboard SSE/streaming components to key modules if they matter for current development.
- Clarify that `graphify update .` performs deterministic/code refresh; full semantic docs extraction still requires `/graphify --update` with extraction agents.
- Keep the existing lazy-repair warning strong: do not add peer polling loops, periodic ghost checks, or eager persistence.

## Suggested Follow-Up Issues

I could not create Beads issues because Beads is not initialized correctly in this worktree. Suggested follow-ups for whoever owns task tracking:

- Decide whether `graphify-out/graph.json` and `graphify-out/graph.html` should remain tracked or move to generated artifacts/release assets.
- Fill `docs/reference/architecture.md`; it is currently a stub despite the README/CLAUDE architecture content being rich.
- Add a short public roadmap section using session-native wording above, clearly marked planned/in-progress.
- Document ACP under an experimental/developer section, not in the stable quickstart path.
