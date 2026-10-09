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

The logged-out landing page plays a decorative dotted orb → Repowire R reveal once on arrival, then holds the mark. It does not reflect peer or connection status. Reduced-motion preferences show the static R, which also remains visible without JavaScript. The relay-key form is available throughout.

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

The hosted deploy workflow selects `oauth.proxyMode=gclb` and reads the gateway's
forwarding IP into `oauth.gclbForwardingIP`. Registration limits verify the GFE
socket source and Google's appended `X-Forwarded-For` suffix. Cloudflare's visitor
header is trusted only when the load balancer's recorded source is Cloudflare;
direct-origin requests are limited by the IP Google recorded instead. No
Cloudflare-only firewall or Cloud Armor policy is required for this selection.

The chart defaults to `direct` for self-hosters. Startup logs state the configured
mode. After rollout, verify registration budgets through Cloudflare and directly
through the gateway; the pod-level header path has not been measured on the live
service yet. Adding a sidecar or changing the gateway type requires revisiting
this trust chain. Health checks, local daemon endpoints, and established MCP
connections do not pass through the registration limiter. See the
[relay MCP reference](../reference/relay-mcp.md) for header fallbacks and CIDR
maintenance.
