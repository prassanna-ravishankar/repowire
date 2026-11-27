# WebSocket Events

Repowire peers use WebSocket delivery for live messages and daemon events. The daemon remains the routing hub; transports decide how to present those messages to a runtime.

## Message families

- `ask` — tracked question that must be closed with `ack`.
- `notify` — fire-and-forget message.
- `broadcast` — mesh-wide announcement.
- Dashboard events — timeline, peer, chat-turn, tool-call, and operational events. Includes `peer_contradiction` events, emitted (once per transition) when lazy repair detects a peer in a self-inconsistent state such as online-but-no-WebSocket, and `ask_delegated` (`correlation_id`, `from`, `to`, `asker`, `note`, `hops`) when an ask is [delegated](../concepts/message-types.md#delegation). Two events record how spawned agents end, without changing peer state (`rfcs/agent-container.md`, step 2). `agent_exit_observed` (`peer_id`, `pid`, `pane_id`, `peer_status`) is emitted when a spawned agent's process exits. `exit_signal_checked` (`signal`, `peer_id`, `pid`, `agent_alive`, `watched`) is emitted when a tmux pane-died or session-closed signal offlines a spawned peer, and says whether its agent was still running.

`ask` and `notify` frames to an agent peer carry an optional `message_id`, the recipient's [message record](mcp-tools.md#messages); renderers show it as `message-id="..."` on the `<peer-message>` tag. For a peer in pull delivery the frame's `text` is a one-line nudge naming that id instead of the body.

A `connect` frame may carry `native_name`, the runtime's own session name; the `connected` reply's `display_name` is then that name, normalized (OpenCode and Pi send it). Renames are reported over HTTP (`POST /session/update`); there is no rename frame.

The ws-hook `connect` frame advertises `hook_version` and `capabilities` (e.g. `delivery_receipts`) so the daemon can report per-peer inbound health and receipt support; capability is also inferred from an observed delivery acknowledgement.

## Compatibility

The exact event payloads are transport-facing internals unless documented by an HTTP route or MCP tool. Prefer MCP tools for agent actions and the HTTP API for external integrations.

## Related

- [Message types](../concepts/message-types.md)
- [MCP tools](mcp-tools.md)
- [Operate: transports](../operate/transports.md)
