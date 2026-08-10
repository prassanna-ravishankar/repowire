# Go the full surface + HTTP MCP

Status: implemented on `feat/daemon-go` (2026-07-10). Tracked in
repowire-53c (daemon), repowire-76o (hooks), repowire-sfh (HTTP MCP), and
repowire-jx8 (CLI/distribution).

## Why

The Go hub port (feat/daemon-go) proved the language fit: the daemon came out
structurally better than the Python original (typed PeerID/DisplayName, a
compiler-enforced peer↔state seam, real shutdown lifecycles). Two pressures
push the rest of the surface the same way:

- **Hooks fire as fresh subprocesses on every agent turn** (SessionStart /
  Stop / UserPromptSubmit). Each invocation pays the Python interpreter +
  import tax (~100–300 ms) versus ~5 ms for a static Go binary. This is the
  one place users feel the runtime on every single turn.
- **Distribution.** A single static binary removes uv/venv variance and the
  "hooks run from the installed package" reinstall foot-gun. During the
  transition, wheels keep wrapping the binary so `uv tool install repowire`
  continues to work (platform wheels already ship the Go hub via the hatch
  hook).

What stays as-is: the channel server and web dashboard are TypeScript and
remain so (transports are client-side; the daemon philosophy is unchanged).

## Implemented result

The native binary now owns config loading, SQLite bootstrap/migrations, resume
safety, the full daemon route surface, ACP subprocess routing and permission
relay, hooks/ws-hook/chat streaming, runtime installers, service management,
and the CLI. The wheel entry point execs the binary; it no longer selects a
Python daemon fallback.

The dashboard and channel server remain TypeScript. The hosted relay server and
Telegram/Slack peers remain separate Python deployments/clients; they are not
part of the local substrate and connect to the Go daemon unchanged.

## Completed phases

1. **Daemon independence:** Go loads config and owns state migrations/resume safety.
2. **Hooks:** session/stop/prompt/notification/pre-tool hooks, ws-hook, chat streaming, and lifecycle hooks run in Go.
3. **HTTP MCP:** all 31 tools live daemon-side at `/mcp`.
4. **CLI/distribution:** the Go CLI and embedded runtime assets ship in platform wheels; the Python entry point is only a native-binary launcher.

## HTTP MCP: /mcp on the daemon + stdio identity shim

Move all MCP logic behind a streamable-HTTP `/mcp` endpoint on the (Go)
daemon. MCP-over-HTTP is JSON-RPC POSTs plus optional SSE — both already
served by the hub and carried by the relay tunnel unchanged.

**The identity constraint (the load-bearing design point).** The stdio MCP
server is not just transport: it is a per-session identity shim. Each agent
spawns its own MCP process, which inherits that agent's env and cwd — that is
what whoami/backend detection/session binding run on. A single shared local
HTTP endpoint erases this: the daemon sees N identical connections. In-protocol
inference does not recover it locally — `clientInfo` says "claude-code" for
every session, and MCP roots give a project path, but same-path peers (spawned
reviewers) are the canonical collision; path/cwd alone is not identity (that
is the registry's core philosophy). There is also no reliable way to inject a
per-session token into static MCP config headers.

**Resolution: keep a paper-thin stdio shim at the edge.** Spawned per-agent as
today, so it inherits env+cwd; it stamps identity headers
(`X-Repowire-Peer` plus the runtime birth-certificate proof) and blindly proxies
JSON-RPC to the daemon's `/mcp`. It ships as `repowire mcp` in the same binary. The
stdio hop survives *because it is the identity channel*, not for protocol
reasons. All tools, registration, and binding logic live daemon-side, once.

**Remote access composes later** (not in scope now): the same `/mcp` rides
the relay tunnel. Remote callers without shim headers register as
steering-peers (the @telegram/@dashboard class: no cwd, no pane,
connection-based liveness — the mesh already supports this as first-class).
Identity chain remote: bearer token → mesh/machine; `Mcp-Session-Id` → peer;
`clientInfo` → backend; roots (when offered) → project. Reconnects adopt by
identity tuple (token + clientInfo + root), the same move as the Go hub's
restart mapping adoption. What cannot be inferred remotely — pane, PID,
branch — maps onto the existing runtime-evidence ceiling: no destructive pane
proof means kill/restart refuses, which is correct for a peer whose runtime we
cannot see. Destructive tools (spawn_peer/kill_peer/schedule_*) require an
elevated token scope; the default remote scope is the steering set
(list_peers, ask, ack, notify, broadcast, review_queue). claude.ai custom
connectors additionally need OAuth 2.1 + dynamic client registration on the
relay — that is the bulk of any remote-phase effort and lives relay-side.
