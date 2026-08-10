# Concepts

The mental model behind Repowire. Read once, refer rarely.

- [Peers and circles](peers-and-circles.md) — what a peer is and how circles scope routing.
- [Sessions](sessions.md) — durable work context versus live runtime executors.
- [Agent backends](agent-backends.md) — Claude Code, Codex, Gemini, Antigravity, OpenCode, Pi, and backend identity.
- [Bridges](bridges.md) — runtime-side adapters from native session APIs into the mesh.
- [Transports](transports.md) — hooks, plugins, channel/ACP, relay, and why routing stays transport-neutral.
- [Message types](message-types.md) — `ask`, `ack`, `notify`, `broadcast`.
- [Jobs and schedules](jobs-and-schedules.md) — delayed messages, recurring work, and durable jobs.
- [Personas](personas.md) — SOUL.md identity files for orchestrator and persona sessions.
- [Lazy repair](lazy-repair.md) — why repowire has no polling loops.
- [Control surfaces](control-surfaces.md) — dashboard, Telegram, Slack as peers.
- [Orchestrator pattern](orchestrator.md) — a peer whose job is coordinating other peers.
- [Peer identity lifecycle](peer-identity-lifecycle.md) — registration, reconnect, stale fields, and routing observability.
- [Session-native roadmap](session-native-roadmap.md) — where the v0.14 architecture train is headed.
