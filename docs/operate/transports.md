# Transports

## What they do

Transports are runtime-specific delivery paths. The daemon routes at the peer/message level; transports handle how a given runtime receives and reports messages. A [bridge](../concepts/bridges.md) is the runtime-side adapter that provides a native transport.

## Claude hooks + MCP transport

This is the default path for Claude Code 2.1.224 and newer:

- Native Go lifecycle hooks register peers, update status, extract transcript/chat turns, and fetch pending ask reminders.
- `repowire mcp` preserves the runtime's peer certificate over stdio and proxies outbound tool calls to the daemon's localhost `/mcp` implementation.
- Live inbound messages are delivered through Claude's authenticated per-session native inbox. A missing or failed inbox is reported as a delivery failure; Repowire does not synthesize keystrokes as a fallback. The hook is bound to the owning agent PID and exits when that process disappears, so an orphaned hook cannot keep a dead peer's daemon socket alive indefinitely.
- Native Claude inbox writes return an `accepted` receipt and can reach busy sessions. Claude's own `crossSessionInbound` controls may still hold or refuse the message; Repowire never changes those controls.
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
The bridge restores mesh instructions with App Server history injection, binds
per-call `_meta.threadId` MCP identity through a daemon-minted certificate,
and preserves peer provenance, ask correlation, safe uploaded-image input,
tool-call summaries, and handoff state. On the idle transition it reads the
completed turn once, since App Server item events are client-scoped; no polling
is involved.
The ordinary Codex TUI remains visible. Tmux is optional placement/lifecycle
evidence and is not the delivery channel. A reminder-only Stop hook resurfaces
asks that remain open after a turn; it does not duplicate App Server lifecycle
or chat handling. Codex releases without a Unix App Server listener are not a
supported inbound-delivery path.

## Plugin and extension transports

OpenCode uses a TypeScript plugin with a persistent WebSocket connection and the plugin API's `dispose` lifecycle. Pi uses a native Repowire extension with Pi's session lifecycle and `sendUserMessage`/`steer` delivery APIs. Neither uses tmux for message delivery. Pane-less local sessions authenticate from Repowire's local config, pre-register over HTTP, and share a stable project-derived circle; tmux/spawn placement wins when present.

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
