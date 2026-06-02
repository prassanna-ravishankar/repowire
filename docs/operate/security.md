# Security posture

The relay is the only piece of repowire that can sit between you and your agents. Knowing what it sees matters.

## What the relay sees

The relay terminates the dashboard's HTTPS, then tunnels HTTP requests over the daemon's outbound WebSocket. **Tunnel payloads are not encrypted end-to-end** — the relay process can read every request body and response body that flows through it, including:

- Agent messages (`ask`, `notify_peer`, `broadcast` text).
- Mesh events (chat turns, tool calls).
- Attachments (uploaded via `POST /attachments`).
- Peer state (names, paths, descriptions).

For the hosted relay at `repowire.io`, this means the operators *could* see your traffic. They commit to not doing so, but the trust is operational, not cryptographic.

## What the relay doesn't see

- **Local-only traffic.** A daemon running without `relay.enabled: true` never connects to the relay. Mesh traffic stays on `127.0.0.1:8377`.
- **Other users' traffic.** API keys scope by user id; daemons in different scopes cannot reach each other.
- **MCP-server traffic.** Default MCP tool calls travel agent → MCP stdio → daemon HTTP, locally. The opt-in HTTP MCP endpoint is also localhost-only and is not tunneled through the hosted relay.

## Authentication

- The daemon authenticates to the relay with `relay.api_key`. The key is auto-generated and stored in `~/.repowire/config.yaml`.
- The dashboard authenticates to the relay with a cookie set after submitting the same API key at `/auth`. Possession of the key grants dashboard access; treat it like a password.
- Local-daemon `daemon.auth_token` is independent and gates the local WebSocket / HTTP API. `repowire setup --http-mcp` generates one because HTTP MCP requires bearer auth by default. Set or rotate it manually if other processes on the machine should not have free access.

## Trust boundaries

| Boundary | What protects it |
| --- | --- |
| Browser ↔ relay | TLS (you trust the relay's certificate) |
| Relay ↔ daemon | TLS-over-WSS (you trust the relay's network and process) |
| Daemon ↔ agent (MCP) | Local stdio by default; optional HTTP MCP is localhost-only with bearer auth |
| Daemon ↔ agent (hooks) | Local HTTP on `127.0.0.1`; gated by `daemon.auth_token` if set |

## Hardening recommendations

- **Set `daemon.auth_token`** if you share the machine with other users.
- **Rotate `relay.api_key`** on suspected compromise. See [relay key rotation](../troubleshooting/relay-keys.md).
- **Prefer the self-hosted relay** for sensitive workloads. The trust model is the same; the operator is you. See [Relay access](../use/features/relay-access.md#common-workflows).
- **CORS**: the daemon restricts CORS to localhost origins and `repowire.io` when the relay is enabled. Don't widen this unless you've thought about what's behind the new origin.

## What we don't do (yet)

- End-to-end encryption of tunnel payloads.
- Per-tool ACLs (any peer with a name can be addressed by any other peer with the same circle and key).
- Audit logging on the relay beyond connection-level events.

These are not present in the MVP. If your threat model requires them, the right call is to keep `relay.enabled: false` and run repowire purely local.
