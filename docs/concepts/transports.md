# Transports

Transports are runtime-specific delivery adapters below the user-facing routing model. Peers still share the same message semantics: `ask`, `ack`, `notify_peer`, and `broadcast`.

## Model

The daemon routes at the peer and message level. A transport handles how a specific runtime receives inbound messages, reports lifecycle, exposes outbound tools, and returns chat turns.

This separation keeps routing transport-neutral: higher-level tools address peers and sessions, not hook files, tmux panes, plugins, or relay sockets.

## Normalization

Agent runtimes expose different lifecycle hooks, event names, and delivery affordances. Repowire normalizes them into:

- peer registration and liveness state,
- a transport router for live delivery,
- ask/ack lifecycle state,
- chat-turn events for human surfaces,
- session bindings where backend runtime ids are available.

## Related

- [Operate: transports](../operate/transports.md)
- [Hook payloads](../reference/hook-payloads.md)
- [Message types](message-types.md)
- [MCP tools](../reference/mcp-tools.md)
