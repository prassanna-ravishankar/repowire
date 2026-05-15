# Self-hosting the relay

The relay is the same code as `repowire.io`. Run your own when you want remote access without depending on the hosted service.

## Run

```bash
repowire relay start --port 8000
```

That starts the relay's FastAPI app on the given port. Put it behind your own TLS terminator (nginx, Caddy, a load balancer) on a domain you control.

## Point your daemon at it

```yaml
# ~/.repowire/config.yaml
relay:
  enabled: true
  url: "wss://relay.yourdomain.example"
  # api_key auto-generated on first run
```

Then restart the daemon. The daemon opens an outbound WebSocket to your relay; from then on the flow is identical to the hosted version.

## What you need to operate

- TLS termination (the relay does not handle certs).
- A way to keep the relay process running (systemd unit, container orchestration, whatever fits).
- DNS pointing at the relay host.

The relay holds no persistent state per daemon — it's effectively a router with API-key auth. Restarting it disconnects active daemon WebSockets, which then reconnect on their own.

## Multiple daemons, one relay

A single relay can serve many daemons. Each daemon is scoped by its API key's user id; daemons sharing a user id can route messages to each other across the relay. Daemons in different user scopes cannot see each other.

## Why self-host

- Compliance: your data path stays on infrastructure you control.
- Latency: a relay co-located with your fleet is faster than a hop to `repowire.io`.
- Independence: no dependency on a hosted service's uptime.

The hosted relay is convenient; the self-hosted relay is the same software, none of the trust assumptions about a third party.

## See also

- [Hosted relay](hosted.md) for the topology and auth flow (identical here).
- [Security posture](security.md) for what the relay sees and what it doesn't.
