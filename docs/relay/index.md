# Relay

Remote access for your daemon. The daemon opens an outbound WebSocket to a relay; the relay bridges WS traffic between daemons on different machines and tunnels HTTP back to your local dashboard.

- [Hosted relay](hosted.md) — `repowire setup --relay` against `repowire.io`.
- [Self-hosting](self-hosting.md) — running `repowire relay start` yourself.
- [Security posture](security.md) — what the relay can and can't see.
