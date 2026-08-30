# Session-native roadmap

Repowire's current public model is peer-oriented: running agent processes register as peers, and tools route `ask`, `ack`, `notify`, `broadcast`, and schedules between those peers.

The v0.14 architecture train is moving toward a **session-native mesh**. This is roadmap and design direction, not a statement that all of it has shipped.

## Direction

- **Session-first mesh.** Sessions become the durable unit of work, with peers acting as live runtime executors.
- **Transport-neutral routing.** Ask/notify delivery now goes through a transport router. WebSocket hooks, experimental ACP, relay, and future transports continue moving toward the same message/control boundary.
- **Timeline-centered dashboard.** Persisted history and realtime events converge into one session timeline instead of separate live/history views.
- **Shared command surface.** Controls such as send message, fork work to another backend/model, resume, schedule, and approvals target sessions and can be reused from dashboard, MCP, Telegram, and other surfaces.
- **Search as large recall.** SQLite-backed session/timeline search should carry detailed historical recall, while orchestrator markdown memory remains a small curated procedure layer.
- **Compatible v0.14 slices.** The architecture changes land incrementally while preserving current hooks, MCP tools, HTTP routes, and dashboard workflows.
- **Human approval path.** Permission and plan approval events become first-class timeline/control events instead of transport-specific callbacks.

The ask/notify transport-router extraction, SQLite session bindings, dashboard session timeline, conversation search, and first session controls have landed. The remaining planned sequence expands the shared session command surface, centralizes more runtime lifecycle behavior, and turns approval events into richer dashboard and control-surface workflows.

## Memory and audit hooks

Session-native storage should make orchestrator learning auditable without bloating prompt memory. Future session records should be able to point at the memory/pattern snapshot used when a session started or resumed, and memory/procedure changes should appear as timeline events when proposed or applied.

This keeps the layers distinct:

- **SQLite/session search:** long-tail recall for past discussions, asks/acks, releases, logs, transcript-derived chat turns, and why a decision happened.
- **Orchestrator markdown memory:** compact user/comms preferences, operational lessons, and dispatch procedures that should affect future coordination.

Do not treat markdown memory as a diary or complete recall system. It should hold only curated rules and procedures that change future behavior.

Do not treat orchestrator memory as global peer memory. Project peers should receive relevant context through explicit briefs, project-local agent files, native runtime skills, or future session/timeline lookup where appropriate.

## What is shipped today

Today, the stable surface is still peer and message based:

- Agents register as peers.
- Circles scope routing.
- `ask` opens a thread; `ack` closes it.
- `notify_peer` and `broadcast` are fire-and-forget.
- `schedule_create`, `schedule_self`, and `schedule_cron` schedule future deliveries through the daemon.
- The dashboard shows peers, live events, selected peer/session timelines, conversation search, tool calls, and compose controls.

## What not to assume yet

- Do not describe seamless model switching, conversation cloning, approval brokering, or backend changes as completed functionality. Today's dashboard fork shares a workspace and circle while preserving the source peer; it does not transfer backend-native conversation history.
- Do not imply every route/control path is transport-neutral yet.
- Do not call ACP production-ready; treat ACP as experimental.
- Do not claim reliable delivery across all transports.
- Do not describe the roadmap items as fully shipped; v0.14.0 marks the session-native train boundary, not the end of the work.

## Why it matters

The peer model is useful for routing, but humans think in sessions: "this workstream", "this review", "this release train". Session-native architecture lets the dashboard and control surfaces preserve that shape while the runtime executor and transport can change underneath.
