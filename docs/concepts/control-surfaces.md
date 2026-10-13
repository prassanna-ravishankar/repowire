# Control surfaces

Control surfaces are peers that represent humans or human-facing clients. The dashboard, Telegram bot, and Slack bot all route through the same daemon primitives as agent peers: `ask`, `ack`, `notify_peer`, and `broadcast`.

## Human framing

Messages from `@telegram`, `@slack`, and `@dashboard` are human-originated. Repowire frames those inbound messages as direct user instructions at delivery time, so receiving agents do not need to infer that from the display name.

Human surfaces have the `human` role. That role bypasses circle filtering so the human can see and address the mesh without being trapped in one project circle.

## The human inbox

With a human peer registered (the default), the phone bridges are surfaces, not addressees. A message to `telegram`, `slack`, or the human's name is stored in the [human inbox](human-inbox.md) and pushed to every live surface, including any open dashboard. A reply from any surface acks the ask behind it, and read state is shared across surfaces. Agents never need to know which surface the human is using.

## Surface state

Control surfaces are clients of the routing API, not sources of truth. The daemon owns peer state, message routing, ask lifecycle, session bindings, and durable state. If a surface crashes or reconnects, it recovers by reading daemon state rather than reconstructing the mesh itself.

When the daemon unregisters a connected control-surface peer, it also closes
that peer's WebSocket. Long-lived service clients treat the close as a
reconnect signal, so registry removal cannot leave them blocked forever on a
socket the daemon no longer routes.

## Session-targeted controls

Peer-targeted routes address live peer identity. Session-targeted controls address durable `repowire_session_id` bindings. That distinction matters because display names and runtime session ids are not stable routing identities.

The session-control invariant is: resolve from a durable Repowire session binding, then act on the current executor if one exists or resume through backend-native session data if supported. Do not guess from display name, path, or runtime-local ids.

## Related

- [Dashboard](../use/features/dashboard.md)
- [Telegram](../use/features/telegram.md)
- [Slack](../use/features/slack.md)
- [Sessions](sessions.md)
- [Peer identity lifecycle](peer-identity-lifecycle.md)
