# WebSocket Events

Repowire peers use WebSocket delivery for live messages and daemon events. The daemon remains the routing hub; transports decide how to present those messages to a runtime.

## Message families

- `ask` — tracked question that must be closed with `ack`.
- `notify` — fire-and-forget message.
- `broadcast` — mesh-wide announcement.
- Dashboard events — timeline, peer, chat-turn, tool-call, and operational events. Includes `peer_contradiction` events, emitted (once per transition) when lazy repair detects a peer in a self-inconsistent state such as online-but-no-WebSocket.

The ws-hook `connect` frame advertises `hook_version` and `capabilities` (e.g. `delivery_receipts`) so the daemon can report per-peer inbound health and receipt support; capability is also inferred from an observed delivery acknowledgement.

## Compatibility

The exact event payloads are transport-facing internals unless documented by an HTTP route or MCP tool. Prefer MCP tools for agent actions and the HTTP API for external integrations.

## Related

- [Message types](../concepts/message-types.md)
- [MCP tools](mcp-tools.md)
- [Operate: transports](../operate/transports.md)
