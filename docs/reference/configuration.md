# Configuration

Config lives at `~/.repowire/config.yaml`. `repowire setup` creates the file with `0600` permissions.

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "rw_local_..."
  mcp_http:
    enabled: false
    bind: "localhost-only"
    require_auth: true
    allow_unauthenticated_localhost: false
    allow_dangerous_tools: false
  spawn:
    allowed_commands: []
    allowed_paths: []
```

## `daemon.auth_token`

Optional local bearer token for daemon HTTP routes, WebSocket connections, hooks, and the opt-in HTTP MCP endpoint. `repowire setup --http-mcp` generates one automatically if missing. Treat it as a local password; rotate it by replacing the value and restarting the daemon service.

## `daemon.mcp_http`

Experimental Streamable HTTP MCP endpoint mounted at `http://127.0.0.1:8377/mcp`.

- `enabled`: opt in to mounting `/mcp`. Default: `false`.
- `bind`: only `localhost-only` is supported. If `daemon.host` is not `127.0.0.1`, `::1`, or `localhost`, `/mcp` is not mounted.
- `require_auth`: require `Authorization: Bearer <daemon.auth_token>`. Default: `true`.
- `allow_unauthenticated_localhost`: development-only escape hatch that disables bearer auth when `require_auth` is also disabled. Do not use on shared machines.
- `allow_dangerous_tools`: allow lifecycle/admin MCP tools over HTTP MCP. Default: `false`; spawn, kill, and schedule mutation stay disabled.

HTTP MCP is never exposed through the hosted relay. The default stdio MCP server installed by `repowire setup` is unchanged and remains the stable path for agents.
