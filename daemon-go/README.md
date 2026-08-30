# daemon-go

Repowire's production substrate: daemon, CLI, runtime hooks, HTTP MCP server,
and the per-agent stdio identity shim. The Go daemon owns the schema-v12
SQLite database, including bootstrap and migrations; Python is not required on
the daemon, CLI, hook, or MCP request path once the binary is running.

The hosted relay server, Telegram/Slack bot peers, dashboard, and experimental
Claude channel server remain separate clients/deployments. They speak the same
HTTP/WebSocket protocol to this daemon.

## Layout

- `cli/` — setup, service management, peer/job/schedule/session commands, and runtime installers
- `hooks/` — runtime event normalization, ws-hook supervision, native Claude inbox delivery, transcript extraction
- `mcpstdio/` — newline JSON-RPC proxy that stamps the calling peer identity and forwards to `/mcp`
- `hub/` — HTTP/WebSocket routes and the complete 31-tool MCP server
- `service/` — delivery, asks, ACP, spawn/resume, jobs, schedules, and permissions
- `peer/` — typed identity registry, lifecycle FSM, lazy reconciliation
- `state/` — SQLite bootstrap, migrations, and durable stores
- `relay/` — outbound hosted-relay client
- `proto/` — wire types, including distinct `PeerID` and `DisplayName`
- `main.go` — command dispatch and daemon wiring

## MCP transport

All MCP tool logic lives at the daemon's localhost-only `/mcp` endpoint. Agent
runtimes still launch `repowire mcp` over stdio, but that process is deliberately
paper-thin: it inherits the runtime's session environment/cwd, resolves the
canonical peer identity, stamps `X-Repowire-Peer`, and proxies JSON-RPC to
`/mcp`. Removing stdio entirely would erase identity for same-path peers.

Direct HTTP MCP clients may call `/mcp` with the local bearer token. Without the
identity shim they act as `mcp-http`, and lifecycle/admin tools remain disabled
unless explicitly allowed. `/mcp` is rejected by the hosted-relay tunnel.

## Build and verify

```bash
cd daemon-go
go build -o repowire .
go test ./...
go test -race ./...
go vet ./...
```

Run the daemon in the foreground with `./repowire serve`. Normal installations
use `repowire setup`, which configures the local HTTP MCP endpoint, installs the
identity shim and hooks/plugins for detected runtimes, and installs the user
service.

OpenCode, Pi, and channel TypeScript assets are embedded into the binary so
their installers work from a standalone build.
