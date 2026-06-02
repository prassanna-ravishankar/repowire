# Relay

## What it runs

The relay is the optional hosted or self-hosted bridge for remote dashboard access and cross-machine traffic. Local daemons connect outbound over WSS; the relay tunnels dashboard HTTP/SSE calls and bridges WebSocket traffic.

## Hosted relay

Enable the hosted relay with:

```bash
repowire setup --relay
```

Then use `https://repowire.io/dashboard` for remote dashboard access.

The relay landing page accepts the relay key from setup and redirects to the dashboard when the matching daemon is connected. Missing, invalid, or disconnected keys return to the landing page with an inline error.

## Self-hosted relay

Self-hosting runs the same relay server under your own deployment and points the daemon at your relay URL.

## Related

- [Relay access](../use/features/relay-access.md)
- [Auth and security](security.md)
