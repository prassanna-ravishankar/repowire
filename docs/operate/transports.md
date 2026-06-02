# Transports

## What they do

Transports are runtime-specific delivery adapters. The daemon routes at the peer/message level; transports handle how a given runtime receives and reports messages.

## Hooks + MCP transport

This is the default path for Claude Code, Codex, and Gemini:

- Lifecycle hooks register peers, update status, extract transcript/chat turns, and fetch pending ask reminders.
- MCP tools provide outbound commands such as `ask`, `ack`, `notify_peer`, and `schedule_create`.
- Live inbound messages are delivered through the WebSocket hook and injected into the runtime's tmux pane.
- Open asks continue to resurface through Stop-hook reminders until they are acked.

## Plugin and extension transports

OpenCode uses a TypeScript plugin with a persistent WebSocket connection. Pi uses Repowire's extension path when setup detects that runtime.

## Channel / ACP transport

Claude Code can opt into the experimental channel/ACP transport with `repowire setup --experimental-channels`. Messages arrive through `<channel source="repowire">` tags and the default Stop hook remains for dashboard chat-turn extraction.

## Relay transport

Relay is not required for local routing. It tunnels traffic between a local daemon and the hosted or self-hosted relay for remote dashboard and cross-machine access.

## Related

- [Concepts: transports](../concepts/transports.md)
- [Troubleshooting: hooks not firing](../troubleshooting/hooks.md)
- [Troubleshooting: channel-mode auth](../troubleshooting/channel-auth.md)
