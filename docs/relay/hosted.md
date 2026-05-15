# Hosted relay

The hosted relay at [repowire.io](https://repowire.io) is the easiest way to reach your local mesh from a different machine or a phone browser.

## Enable

```bash
repowire setup --relay
```

Or, if you already ran setup:

```bash
repowire serve --relay
```

The daemon opens an outbound WebSocket to `wss://repowire.io/ws/relay`. An API key is auto-generated on first run and written to `~/.repowire/config.yaml` under `relay.api_key`.

## What you get

- `https://repowire.io/dashboard` — your dashboard, served by the relay's static export, with API calls tunneled to your local daemon.
- Cross-machine mesh — daemons on different machines can route messages to each other via the relay if they share a user scope (auth key tied to the same account).
- An SSE event bridge so the remote dashboard sees mesh events in real time.

## Mechanics

```
Browser ──HTTPS──> repowire.io ──┐
                                 │  cookie session (set via /auth)
                                 ▼
Browser <──HTTPS── repowire.io ──── tunneled HTTP ────> your daemon
                          ▲
                          │ outbound WSS (always-on)
                          ▼
                      your daemon
```

The daemon is the active party — it holds an outbound WebSocket to the relay. When you browse `repowire.io/dashboard`, the relay tunnels API calls (peers, events, ask, ack, attachments, ...) over that same WebSocket back to your daemon. The relay never opens a connection *to* your machine.

The relay also serves the dashboard's Next.js static export directly. Only API and SSE traffic tunnels — the HTML, JS, and CSS load from the relay's own CDN.

## Auth flow

1. Visit `https://repowire.io`.
2. Paste your API key into the auth form.
3. The relay sets a cookie tied to that key's user scope.
4. Browsing `/dashboard` from then on is cookie-authenticated.

Your API key is in `~/.repowire/config.yaml` under `relay.api_key`. Keep it secret — see [security posture](security.md).

## Reconnects

The daemon's relay client auto-reconnects on disconnect with exponential backoff. Disconnections are normal (Wi-Fi blips, suspended laptops); the WebSocket comes back within a few seconds of network return. You'll see a short gap of "daemon offline" on the relay-hosted dashboard during the disconnect.

## Disable

```bash
repowire setup     # re-run without --relay
```

That removes the relay startup from the installed service. To also delete the API key:

```bash
# stop the daemon, then:
# edit ~/.repowire/config.yaml and remove the `relay:` block
```

## See also

- [Self-hosting](self-hosting.md) if you don't want to depend on `repowire.io`.
- [Security posture](security.md) for what the relay can and can't see.
- [Troubleshooting: relay key rotation](../troubleshooting/relay-keys.md).
