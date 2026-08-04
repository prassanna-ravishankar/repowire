# Transports

Transports are runtime-specific delivery paths below the user-facing routing model. Peers still share the same message semantics: `ask`, `ack`, `notify_peer`, and `broadcast`.

## Model

The daemon routes at the peer and message level. A transport handles how a specific runtime receives inbound messages, reports lifecycle, exposes outbound tools, and returns chat turns.

This separation keeps routing transport-neutral: higher-level tools address peers and sessions, not hook files, tmux panes, plugins, or relay sockets.

When a runtime exposes a native session API, a [bridge](bridges.md) is the
runtime-side component that translates that API into the mesh. Bridge names
describe components; transport names describe the delivery path and its
capabilities.

## Normalization

Agent runtimes expose different lifecycle hooks, event names, and delivery affordances. Repowire normalizes them into:

- peer registration and liveness state,
- a transport router for live delivery,
- ask/ack lifecycle state,
- chat-turn events for human surfaces,
- session bindings where backend runtime ids are available.

## Related

- [Operate: transports](../operate/transports.md)
- [Bridges](bridges.md)
- [Hook payloads](../reference/hook-payloads.md)
- [Message types](message-types.md)
- [MCP tools](../reference/mcp-tools.md)
