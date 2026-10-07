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

## Diagnosing a disconnected relay

If the dashboard login bounces back to the landing page with `no_daemon`, or peers do not appear in the remote dashboard, the daemon's relay client is not connected — the relay has no daemon to bind the session to. Check live relay state on the local daemon:

```bash
curl -s http://127.0.0.1:8377/health | jq .relay
```

The `relay` block reports the real connection, not just the config flag:

- `status: connected` — the relay client holds a live WebSocket.
- `status: connecting` — the reconnect loop is running but not yet connected.
- `status: down` — relay is enabled but not connected; `last_error`/`last_error_at` carry the cause. Hitting `/health` also lazily relaunches the loop if it had stopped.
- `status: disabled` — relay is not enabled in config.

`relay_mode` remains the config intent (`relay.enabled`); `relay.status` is the truth. The relay client keeps an application-level keepalive ping so half-open connections are detected and reconnected rather than silently wedging.

## Related

- [Relay access](../use/features/relay-access.md)
- [Auth and security](security.md)

## Remote MCP and OAuth

See [Relay MCP](../reference/relay-mcp.md) for the OAuth connection flow, remote tools, token refresh and revocation, and portable/Claude plugin installation. The remote surface is a scoped companion to the local daemon MCP tools.

OAuth-enabled relay deployments use one replica and a retained RWO volume. The
Recreate rollout briefly disconnects clients while the relay pod restarts; durable
grants survive, and daemons reconnect through their existing relay transport.

The hosted chart marks its route as Cloudflare-proxied, but this repository does
not establish that the gateway rejects direct origin traffic or sanitizes
`CF-Connecting-IP`. Before rolling out public OAuth registration, verify that
restriction and then enable `oauth.trustCloudflareIP`. It defaults to `false` for
safe self-hosting. Until configured, clients routed through one gateway share its
registration bucket, so one caller can exhaust registration capacity temporarily.
The application cannot safely infer proxy trust from the presence of a header.
