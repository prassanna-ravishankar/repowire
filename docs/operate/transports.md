# Transports

## What they do

Transports are runtime-specific delivery paths. The daemon routes at the peer/message level; transports handle how a given runtime receives and reports messages. A [bridge](../concepts/bridges.md) is the runtime-side adapter that provides a native transport.

## Hooks + MCP transport

This is the default path for Claude Code and Gemini:

- Native Go lifecycle hooks register peers, update status, extract transcript/chat turns, and fetch pending ask reminders.
- `repowire mcp` preserves the runtime's peer certificate over stdio and proxies outbound tool calls to the daemon's localhost `/mcp` implementation.
- Live inbound messages are delivered through the WebSocket hook and injected into the runtime's tmux pane. The hook is bound to the owning agent PID and exits when that process disappears, so an orphaned hook cannot keep a dead peer's daemon socket alive indefinitely.
- Non-human deliveries are injected inside a `<peer-message>` envelope carrying sender, type, target, and correlation id when applicable. Dashboard, Telegram, and Slack messages remain direct human instructions.
- Open asks continue to resurface through Stop-hook reminders until they are acked.

Deregistration is layered, strongest signal first:

1. **SessionEnd (intent)** — a clean quit posts a terminal offline immediately (`reason=session_end`). `/clear` is skipped: the follow-up SessionStart rebinds the same pane milliseconds later.
2. **Agent-pid watcher (crash)** — the ws-hook watches its owning agent PID and, when it disappears (including SIGKILL), posts a terminal offline (`reason=agent_exited`) before exiting — this covers backends without a session-end hook.
3. **Liveness pings (backstop)** — lazy repair demotes a connected pane peer only after three consecutive honest `pane_alive=false` verdicts; inconclusive checks (tmux/ps hiccups) are never counted.

A *terminal* offline retires the peer identity: the daemon severs its websocket and rejects reconnects claiming that peer_id unless they prove a live agent PID. A fresh SessionStart (which always carries one) reclaims the identity; a leftover orphan hook cannot. At startup the daemon additionally sweeps orphaned ws-hook processes whose agent is conclusively gone.

Lazy repair treats a recorded agent PID as authoritative runtime evidence: if that PID is gone, a leftover tmux pane or shell is not enough to keep the peer online. Peers without a recorded agent PID can still fall back to live pane evidence.

## Codex App Server transport

Current Codex releases use an independently supervised local App Server and a
Repowire bridge. Threads register before their first prompt. Inbound messages
use `turn/start` while idle and `turn/steer` during a live turn; delivery
receipts are recorded as native thread acceptance, never as pane injection.
The ordinary Codex TUI remains visible. Tmux is optional placement/lifecycle
evidence and is not the delivery channel. A reminder-only Stop hook resurfaces
asks that remain open after a turn; it does not duplicate App Server lifecycle
or chat handling. Older Codex releases without a Unix App Server listener fall
back to hooks + MCP.

## Plugin and extension transports

OpenCode uses a TypeScript plugin with a persistent WebSocket connection. Pi uses Repowire's extension path when setup detects that runtime.

## Claude Channel bridge / ACP transport

Claude Code can opt into the embedded TypeScript Channel bridge with `repowire
setup --experimental-channels`. Messages arrive through `<channel
source="repowire">` tags; non-human content inside the channel is additionally
wrapped in `<peer-message>`. The default Stop hook remains for dashboard chat
turn extraction. Separately, the Go daemon's experiment-gated ACP subprocess
client routes ACP-marked peers and sends tool permission requests through the
same blocking-question path as the dashboard/human surfaces.

## Relay transport

Relay is not required for local routing. It tunnels traffic between a local daemon and the hosted or self-hosted relay for remote dashboard and cross-machine access.

## Related

- [Concepts: transports](../concepts/transports.md)
- [Troubleshooting: hooks not firing](../troubleshooting/hooks.md)
- [Troubleshooting: channel-mode auth](../troubleshooting/channel-auth.md)
