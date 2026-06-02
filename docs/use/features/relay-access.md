# Relay access

## What it is

Relay access lets you reach a local Repowire daemon from another machine or a phone browser without opening inbound ports. The hosted relay runs at [repowire.io](https://repowire.io); you can also self-host the same relay software.

## When to use it

Use relay access when you want the remote dashboard, cross-machine mesh traffic, or phone-browser access while keeping the daemon behind outbound-only connectivity.

Skip the relay when all peers and humans are on the same machine and `http://localhost:8377/dashboard` is enough.

## Setup

Enable the hosted relay:

```bash
repowire setup --relay
```

Or start the daemon with relay enabled:

```bash
repowire serve --relay
```

The daemon opens an outbound WebSocket to `wss://repowire.io/ws/relay`. An API key is generated on first run and written to `~/.repowire/config.yaml` under `relay.api_key`.

Open:

```text
https://repowire.io/dashboard
```

Visit `https://repowire.io`, paste the API key into the auth form, and the relay sets a cookie tied to that key's user scope.

## Common workflows

Run the remote dashboard:

```bash
repowire setup --relay
repowire serve
```

Then open `https://repowire.io/dashboard`. Dashboard HTTP, SSE, and WebSocket traffic tunnel back through the daemon's outbound relay connection.

Self-host the relay:

```bash
repowire relay start --port 8000
```

Put it behind your own TLS terminator and point the daemon at it:

```yaml
relay:
  enabled: true
  url: "wss://relay.yourdomain.example"
```

Restart the daemon after changing the relay URL.

## Commands and API

- `repowire setup --relay` configures hosted relay startup.
- `repowire serve --relay` starts the daemon with relay enabled.
- `repowire relay start --port 8000` starts a self-hosted relay process.
- `repowire status` reports relay configuration and connection state.

The relay serves the dashboard static export. API and SSE traffic tunnel back to the daemon over the outbound relay WebSocket.

## Limits

- The relay never opens a connection to your machine; the daemon is the active party.
- The relay sees tunneled HTTP metadata and traffic needed to route requests. Keep the API key secret.
- Telegram traffic is not proxied by the relay.
- Self-hosting requires TLS termination, DNS, and process supervision.
- Restarting a relay disconnects active daemon WebSockets; daemon clients reconnect with backoff.

## Troubleshooting

- Remote dashboard shows the daemon offline: run `repowire status` locally and confirm relay is connected.
- Auth fails after rotating keys: see [Relay key rotation](../../troubleshooting/relay-keys.md).
- Self-hosted relay works locally but not over the internet: check TLS, DNS, and WebSocket proxying.

## See also

- [Operate: relay](../../operate/relay.md)
- [Auth and security](../../operate/security.md)
- [Relay key rotation](../../troubleshooting/relay-keys.md)
