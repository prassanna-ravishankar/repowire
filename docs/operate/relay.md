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

## Push notifications

The relay sends APNs pushes for the [iOS app](../use/features/ios-app.md), which is the `ios` surface of the [human inbox](../concepts/human-inbox.md). The daemon decides what reaches the human and owns the device tokens (`/push/devices`, stored in its SQLite state); each push frame it sends over the relay WebSocket names its target tokens. The relay only holds the APNs key, and answers each frame with APNs' outcome, which the inbox records as the push's receipt.

Configure the relay with all four variables, or none to leave push off:

| Variable | Value |
| --- | --- |
| `REPOWIRE_RELAY_APNS_TEAM_ID` | Apple developer team id |
| `REPOWIRE_RELAY_APNS_KEY_ID` | Key id of the `.p8` APNs auth key |
| `REPOWIRE_RELAY_APNS_KEY_PATH` | Path to the `.p8` file |
| `REPOWIRE_RELAY_APNS_TOPIC` | The app's bundle id (`io.repowire.app`) |
| `REPOWIRE_RELAY_APNS_BINDINGS_DB` | Optional. SQLite path for device-token bindings, so they survive a relay restart. Without it they are kept in memory. |

The relay sends a push only to device tokens bound to the pushing daemon's relay key. A token binds once the device proves it holds the token; knowing a token is not enough, because Apple treats tokens as identifiers, not secrets. The binding works like this:

1. The app registers its token with `POST /push/devices` through the tunnel.
2. Once the daemon accepts the registration, the relay sends that token a background push (no alert or sound) carrying a random nonce. Only the device that holds the token receives it.
3. The app echoes the nonce to `POST /push/devices/verify {token, nonce}`. The relay answers this route itself; it is not tunneled.
4. The token binds to the registering user only if the nonce matches within 10 minutes and the echo arrives under the same relay key. Each nonce works once.

Another tenant who registers your token triggers a challenge on your phone that they cannot read, so they cannot take the binding. A verified registration under a different key does move the token, since only the phone's holder can complete one. A tunneled `DELETE /push/devices/{token}` unbinds the token for its owner. Challenges count toward the push rate limit.

A token not bound to the pushing daemon's key is skipped and reported back as unbound. The daemon keeps it, and the app re-registers and re-verifies on its next launch. One key holds at most 20 bindings, and the oldest is dropped first.

Each push names an environment per device (`sandbox` for development builds, `production` otherwise). The relay checks a rate limit of 30 pushes per minute per relay key as each frame arrives, and sends from a bounded queue (4 workers, 16 queued per daemon connection). It refuses at once anything over either limit, and sends to at most 10 devices per push. Tokens APNs rejects as unregistered or invalid are reported back to the daemon, which deletes them. Failures are recorded on the message's trace as `surface_failed` with `surface: ios`.

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
