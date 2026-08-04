# Bridges

A bridge is a runtime-side adapter that translates an agent's native session API
into Repowire's peer and message protocol. The daemon remains the only routing
hub; bridges connect to it as ordinary WebSocket clients and do not add
runtime-specific behavior to daemon routing.

Bridges and transports describe different boundaries:

- a **bridge** is the component that speaks both protocols,
- a **transport** is the delivery path and capabilities it provides.

For example, the Codex bridge speaks Codex App Server on one side and Repowire
WebSocket on the other. It exposes the `codex-app-server` transport, including
native thread steering and delivery receipts.

## Lifecycle follows the runtime

Bridges do not share one process model:

- **Codex:** a separately supervised companion owns or reuses the local App
  Server socket. This lets multiple normal Codex TUIs attach while Repowire
  observes and steers their threads.
- **Claude Code Channels:** the channel MCP server is session-owned and starts
  with Claude Code. It pushes mesh events directly into that running session
  and exposes reply and permission-relay tools.
- **OpenCode and Pi:** their plugin and extension are in-runtime bridges because
  those runtimes already provide suitable extension lifecycles.

Tmux can still host, locate, or restart a terminal session, but it is not the
delivery path when a native bridge is active.

## Bridge contract

Each bridge implements only the capabilities its runtime provides:

- register a stable runtime session or thread as a peer,
- report lifecycle and chat events,
- accept inbound mesh messages natively,
- return truthful delivery receipts,
- expose replies or approvals when the native API supports them.

The wire protocol and peer capability metadata are the shared contract. A
common Go interface or common supervisor is unnecessary: most bridges run
inside another runtime and cannot share either implementation or lifecycle.

## Related

- [Transports](transports.md)
- [Architecture](../operate/architecture.md)
- [Operate: transports](../operate/transports.md)

