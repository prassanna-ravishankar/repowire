# Configuration

Config lives at `~/.repowire/config.yaml`. `repowire setup` creates the file with `0600` permissions.

```yaml
daemon:
  host: "127.0.0.1"
  port: 8377
  auth_token: "rw_local_..."
  delivery_queue_ttl_seconds: 86400
  delivery_queue_max_per_peer: 100
  mcp_http:
    enabled: false
    bind: "localhost-only"
    require_auth: true
    allow_unauthenticated_localhost: false
    allow_dangerous_tools: false
  spawn:
    commands:
      claude-code: "claude --dangerously-skip-permissions"
      codex: "codex --dangerously-bypass-approvals-and-sandbox"
      gemini: "gemini --yolo"
      opencode: "opencode"
      pi: "pi"
    profiles:
      codex:
        fast:
          args: ["--model", "gpt-5-mini"]
          description: "Lower-latency Codex peer"
        capable:
          args: ["--model", "gpt-5"]
          description: "More capable Codex peer"
    allowed_paths: []
updates:
  check_enabled: false
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

## `daemon.delivery_queue_*`

Repowire keeps a small SQLite-backed delivery queue for peers that miss a live WebSocket delivery and later poll from a Stop hook or CLI fallback. Live delivery is always attempted first. If the live transport is unavailable, notifications are queued; asks to CLI-fallback peers are queued for one-shot delivery while the open ask thread remains visible through `/asks/pending` until `ack`.

- `delivery_queue_ttl_seconds`: how long a queued delivery remains drainable. Default: `86400` (24 h). Set `0` to disable queued delivery.
- `delivery_queue_max_per_peer`: maximum queued rows retained per peer. Default: `100`. Oldest rows are evicted when the cap is exceeded. Set `0` to disable queued delivery.

Draining is delete-on-read through the Stop hook or `repowire peer deliveries`, so the same queued paste is not replayed indefinitely. Ask reminders are separate: open asks continue to appear through `/asks/pending` until closed.

## `daemon.spawn`

Spawn is disabled until `allowed_paths` and at least one runtime command are configured. `commands` is keyed by backend (`claude-code`, `codex`, `gemini`, `antigravity`, `opencode`, `pi`) and is the single launch profile used by MCP `spawn_peer`, dashboard spawn, backend switching, `repowire peer restart`, and `repowire orchestrator start`.

`profiles` is optional and keyed first by backend, then by a user-defined profile name. Each profile appends structured `args` to the configured backend command; Repowire does not hardcode provider model names. For example, spawning `codex` with profile `fast` runs the configured `daemon.spawn.commands.codex` command plus the profile args. Profile descriptions are informational and may be shown by UIs.

Explicit command overrides still win over profiles. `repowire peer restart` preserves the peer's backend, path, circle, role, and mesh identity, but this slice does not persist the selected profile in peer state. Restart therefore uses the current configured backend command unless a future lane records spawn profile metadata.

`allowed_commands` is a deprecated compatibility field. When present in an older config, Repowire normalizes it into `commands` while loading config; new configs should not add it.

## `updates.check_enabled`

Opt-in release availability checks for `repowire status` and `repowire doctor`. Default: `false`.

When enabled, those commands may query PyPI and report that a newer Repowire release is available. They never install packages, rewrite hooks, restart services, or mutate daemon routing. `repowire update` remains the explicit upgrade path.
