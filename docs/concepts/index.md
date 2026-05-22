# Concepts

The mental model behind repowire. Read once, refer rarely.

- [Peers and circles](peers-and-circles.md) — what a peer is and how circles scope routing.
- [Peer identity lifecycle](peer-identity-lifecycle.md) — registration, reconnect, stale fields, and routing observability.
- [Message types](message-types.md) — `ask`, `ack`, `notify`, `broadcast`.
- [Mesh command UX contract](mesh-command-ux.md) — command ids, JSON and human rendering, and agent-facing invocation rules.
- [Tracked work lifecycle](tracked-work-lifecycle.md) — future daemon-backed work state, status/result/cancel semantics, and boundaries from ask/ack.
- [Lazy repair](lazy-repair.md) — why repowire has no polling loops.
- [Control surfaces](control-surfaces.md) — dashboard, Telegram, Slack as peers.
- [Orchestrator pattern](orchestrator.md) — a peer whose job is coordinating other peers.
- [Session-native roadmap](session-native-roadmap.md) — where the v0.13 architecture train is headed.
