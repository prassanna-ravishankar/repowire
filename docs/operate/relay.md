# Relay

## What it runs

The relay is the optional hosted or self-hosted bridge for remote dashboard and remote MCP access to a local daemon. Local daemons connect outbound over WSS; the relay tunnels dashboard HTTP/SSE calls and relay MCP requests to the daemon. Between an owner's daemons it carries one thing: cross-machine requests (`machine_request` frames), which it re-issues to the target under its own request id, naming the sending machine. See [circles across machines](../concepts/peers-and-circles.md#circles-across-machines) and [multiple machines](../use/features/relay-access.md#multiple-machines).

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

Signing in with a relay key needs a connected daemon, except on a relay with managed agents configured: there a key with no daemon signs in to `/managed`. If the login bounces back to the landing page with `no_daemon`, or peers do not appear in the remote dashboard, the daemon's relay client is not connected, so the relay has no daemon to bind the session to. Check live relay state on the local daemon:

```bash
curl -s http://127.0.0.1:8377/health | jq .relay
```

The `relay` block reports the real connection, not just the config flag:

- `status: connected` — the relay client holds a live WebSocket.
- `status: connecting` — the reconnect loop is running but not yet connected.
- `status: down` — relay is enabled but not connected; `last_error`/`last_error_at` carry the cause. Hitting `/health` also lazily relaunches the loop if it had stopped.
- `status: disabled` — relay is not enabled in config.

`relay_mode` remains the config intent (`relay.enabled`); `relay.status` is the truth. The relay client keeps an application-level keepalive ping so half-open connections are detected and reconnected rather than silently wedging.

## Managed agents control plane

The relay is the control plane for Repowire Managed: it authenticates users, serves the `/managed` and `/admin` pages, proxies to the Cloudflare Worker that runs the agents, bridges the Worker's live events to the browser, and reports costs. Routes are listed in the [relay HTTP reference](../reference/relay-http.md).

Configuration is optional and read from the environment at startup:

| Variable | Purpose |
| --- | --- |
| `REPOWIRE_MANAGED_URL` | Worker control API base URL. |
| `REPOWIRE_MANAGED_CONTROL_TOKEN` | Bearer credential the Worker accepts from the relay. Required with the URL. |
| `REPOWIRE_ADMIN_USERS` | Comma-separated relay user ids (`token-...`, from `GET /api/v1/managed/me`) allowed on `/admin`. |
| `REPOWIRE_CF_ACCOUNT_ID`, `REPOWIRE_CF_ANALYTICS_TOKEN` | Account id and an Account Analytics Read token for actual spend. |

Without the URL and token, managed routes answer 503. The relay refuses to start with a URL but no token.

In the Helm chart, set `managed.secretName` to an existing Secret. Its keys `managed-url`, `managed-control-token`, `admin-users`, `cf-account-id`, and `cf-analytics-token` map to the variables above, and each is optional:

```bash
kubectl -n repowire create secret generic repowire-managed \
  --from-literal=managed-url=https://<worker host> \
  --from-literal=managed-control-token=<token> \
  --from-literal=admin-users=token-<id>
helm upgrade repowire-relay charts/repowire-relay --reuse-values --set managed.secretName=repowire-managed
```

Event bridges and the cost cache live in relay memory. A relay restart drops open event streams, and browsers reconnect.

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
