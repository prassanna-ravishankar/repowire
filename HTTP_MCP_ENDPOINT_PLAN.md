# Plan: Streamable HTTP MCP endpoint on the Repowire daemon

Status: proposal/spec only — no implementation in this branch.

## Goal

Add an opt-in remote MCP surface hosted by the existing local Repowire daemon, for example `http://127.0.0.1:8377/mcp`, using MCP's current **Streamable HTTP** transport. This should be an easier integration path for clients that support remote MCP servers, while preserving today's per-runtime stdio MCP setup for Claude Code, Codex, and Gemini.

## Recommendation

Expose `/mcp` by mounting the existing `FastMCP` app from `repowire/mcp/server.py` rather than duplicating tool definitions.

The current MCP server is already the canonical tool registry: each tool is a thin daemon HTTP client around the same `/peers`, `/ask`, `/ack`, `/notify`, `/spawn`, `/reviews`, and `/schedules` APIs used elsewhere. The installed `mcp` package already exposes `FastMCP.streamable_http_app()`, so the daemon can mount a Streamable HTTP app without rewriting tool handlers.

The main architectural work is not the transport itself; it is caller identity and authorization. The stdio server infers identity from tmux pane metadata, cwd, backend, and hook registration. A remote HTTP MCP client has no tmux pane and may not represent a live agent session. Treat this as a first-class remote control-surface identity, not as an implicit agent.

## Proposed shape

### Endpoint

- `POST /mcp` and `GET /mcp` handled by the mounted Streamable HTTP MCP app.
- MCP headers:
  - Accept and content-type semantics per Streamable HTTP.
  - `MCP-Protocol-Version` honored/forwarded by the MCP implementation where supported.
  - `Mcp-Session-Id` used by the MCP implementation for session continuity after initialization.
- Keep `repowire mcp` stdio unchanged and backwards compatible.

### Enablement

Start opt-in:

```yaml
daemon:
  auth_token: "setup-generated-local-token"
  mcp_http:
    enabled: false
    bind: localhost-only
    require_auth: true
    expose_via_relay: false
```

Implementation can initially omit the full config shape if needed, but product behavior should be explicit: no accidental remote tool exposure. Local-only `/mcp` should not require a human account login; setup should reuse or generate the daemon's local bearer token and provide an easy way to copy client config.

### Tool registry

Use one registry:

```text
repowire/mcp/server.py:create_mcp_server()
      ├── run_stdio_async()              # existing `repowire mcp`
      └── streamable_http_app() mounted  # proposed daemon `/mcp`
```

Avoid copying tool definitions into daemon routes. If HTTP-MCP needs different defaults or restrictions, inject a caller context/auth policy into the existing server factory rather than forking the tools.

## Identity model

### Current stdio behavior

Stdio MCP tools are agent-local. `server.py` lazily resolves/registers the caller via:

- tmux pane lookup (`/peers/by-pane/{pane}`),
- pane runtime metadata,
- cwd + backend matching,
- fallback registration using cwd name, path, backend, and circle.

That is correct for Claude/Codex/Gemini stdio and should remain untouched.

### HTTP MCP behavior

Remote MCP clients need explicit identity. Recommended default:

- Register one daemon-owned peer per HTTP MCP session or token subject.
- Role: `human` for dashboard-like user clients, or `service` for automation tokens.
- Backend: introduce/allow `mcp-http` or reuse an existing non-agent backend only if the schema supports it cleanly.
- Name: default `mcp-http` or a configured/token-provided display name, with daemon suffixing on collision.
- Circle: default from config or token claim; allow explicit circle only when authorized.
- Path/machine: do not infer local cwd from the daemon process. Store empty path or configured label. Machine may be daemon host label, not the caller's remote host unless supplied intentionally.

Identity inputs, in priority order:

1. Auth token metadata/claims if Repowire later supports scoped tokens.
2. Explicit headers/query during MCP initialization, only if authenticated and validated:
   - requested peer name,
   - requested circle,
   - requested role within allowed set.
3. Config defaults.

Do not let unauthenticated HTTP clients become arbitrary peer names. Do not use daemon cwd as caller path.

### Human surfaces

Dashboard, Telegram, and Slack are currently human-role peers with special routing semantics. HTTP MCP should be conceptually similar but distinct:

- It can represent the same human user, but should not silently impersonate `dashboard` or `telegram`.
- If a remote MCP client sends `notify_peer("telegram", ...)`, it should be visible as from `mcp-http`/configured name.
- Circle-bypass semantics for `human`/`service` must be deliberate because they affect mesh-wide visibility and routing.

## Auth and exposure

### Local daemon auth

Require existing `daemon.auth_token` bearer auth for `/mcp` whenever HTTP MCP is enabled. Unlike some current local daemon routes where auth is optional unless configured, `/mcp` should fail closed unless the user explicitly disables auth for localhost-only development.

Minimum policy:

- Localhost bind only by default (`127.0.0.1`, `::1`).
- Require `Authorization: Bearer <daemon.auth_token>` by default.
- `repowire setup` should generate/reuse a local daemon token when enabling HTTP MCP; local-only MCP must not require a human login.
- Provide a low-friction discovery command such as `repowire mcp-url` or `repowire mcp config` that prints the Streamable HTTP URL plus bearer-token client snippet.
- If no token is configured, either generate one automatically during setup or refuse to enable HTTP MCP unless an explicit `allow_unauthenticated_localhost: true` dev flag is set.
- Do not treat localhost as trusted by itself. Local browsers, Electron apps, package scripts, build tools, and unrelated local processes can all reach `127.0.0.1`.

### Local threat model

A localhost-only `/mcp` still exposes a local control plane. The attacker does not need network access if they can run JavaScript in a browser origin that can reach localhost, install a malicious local app, compromise a dev dependency script, or convince the user to run a curl snippet. Because the MCP tool surface can include `spawn_peer`, `kill_peer`, `broadcast`, schedules, peer/path metadata, and future attachment tools, unauthenticated localhost access is too broad for a default.

Safe-subset option for the first slice: expose read/routing tools (`whoami`, `list_peers`, `ask`, `ack`, `notify_peer`) over authenticated HTTP MCP first, and gate lifecycle/admin tools (`spawn_peer`, `kill_peer`, broad schedule management, future attachments) behind a separate scope or config flag. This reduces blast radius while validating Streamable HTTP compatibility.

### Relay

Do not expose `/mcp` through the hosted relay in the first slice.

Reason: MCP over Streamable HTTP gives broad tool access including messaging, spawning, killing, schedules, review metadata, and possibly attachments. The existing relay already tunnels dashboard/API traffic behind relay auth, but MCP clients tend to cache server URLs/tokens and may run outside browsers. Relay exposure needs scoped tokens and a tighter route allowlist.

Future relay support should require:

- explicit `expose_via_relay: true`,
- human login or another remote-user authentication flow,
- scoped MCP token separate from `relay.api_key`,
- route-level deny/allow policy for dangerous tools,
- CORS/origin restrictions for browser-based MCP clients.

### CORS/origin

- Non-browser MCP clients do not need permissive CORS.
- If browser clients are supported, allow only configured origins; do not reuse the broad dashboard CORS list blindly.
- Reject untrusted `Origin` for authenticated browser requests to reduce token exfiltration impact.

## Security risk matrix

| Area | Risk | Initial mitigation |
| --- | --- | --- |
| `spawn_peer` | Starts local processes in allowed paths/commands. Remote abuse can consume resources or launch shells if allowlist is loose. | Keep existing spawn allowlists; consider disabling lifecycle tools for HTTP MCP by default or requiring elevated role/scope. |
| `kill_peer` | Deregisters peers and may kill daemon-spawned tmux panes. | Require authenticated service/orchestrator scope for HTTP MCP, or omit from initial HTTP surface. |
| `attachments` | Upload/download exposes local files under attachment store and consumes disk. | Do not add attachment tools in initial MCP surface; if added later, enforce size/TTL/auth and origin policy. |
| `broadcast`/`notify`/`ask` | Remote client can interrupt all peers or impersonate human intent. | Stable HTTP identity, audit events, circle scoping, no arbitrary from_peer. |
| schedules | Remote client can create persistent future interruptions. | Associate schedule owner with HTTP identity; provide listing/deletion scoped to owner unless elevated. |
| peer listing | Exposes paths, machine labels, descriptions. | Circle-scoped defaults; consider path redaction for human/service HTTP clients exposed beyond localhost. |
| relay | Turns local daemon tools into internet-reachable control plane. | No relay exposure in v1. |

## Tool-surface matrix

| Capability | MCP stdio | HTTP MCP `/mcp` | Pi direct tools | CLI/daemon HTTP |
| --- | --- | --- | --- | --- |
| Transport | stdio child process per runtime | Streamable HTTP mounted on daemon | Pi extension tools over daemon/WebSocket | REST/WS commands |
| Primary caller | Agent session | Remote MCP client / human automation | Pi agent session | Human/operator scripts |
| Identity source | tmux/pane/cwd/backend lazy registration | auth/session/config-provided remote identity | Pi extension session registration | CLI default `cli` or explicit params |
| Tool registry | `create_mcp_server()` | same `create_mcp_server()` | separate Pi tool definitions mirroring mesh API | daemon routes/client |
| Circle default | caller circle; orchestrator may widen | configured/auth identity circle; human/service policy explicit | caller circle | CLI often bypasses or uses explicit circle |
| Dangerous lifecycle tools | available today, guarded by daemon allowlists | consider scoped/disabled initially | available if extension exposes them | available via routes with daemon auth |
| Compatibility | must remain unchanged | additive opt-in | unchanged | unchanged |

## Migration path

1. Add hidden/experimental config and daemon mount for `/mcp` on localhost only.
2. Reuse existing `create_mcp_server()`; preserve `repowire mcp` exactly.
3. Add HTTP caller-context support without changing stdio identity resolution.
4. Ensure local setup generates/reuses `daemon.auth_token` and add an easy config surface (`repowire mcp-url` / `repowire mcp config`) that prints URL + bearer token snippets for clients that support Streamable HTTP.
5. Document client examples for remote-MCP-capable clients, clearly marking stdio as the default stable path.
6. After dogfooding, add setup/status output:
   - whether HTTP MCP is enabled,
   - URL,
   - auth requirement,
   - relay exposure status.
7. Only later consider `repowire setup --http-mcp` or per-client config writers.

## Tests and smoke plan

Unit/integration:

- `create_app()` mounts `/mcp` only when enabled.
- `/mcp` rejects when disabled.
- `/mcp` requires auth when configured and rejects bad/missing bearer tokens.
- Streamable HTTP initialization returns/uses `Mcp-Session-Id` as provided by the MCP library.
- MCP `tools/list` over HTTP includes the same tool names as stdio.
- Calling `whoami` over HTTP returns the configured HTTP identity, not daemon cwd or an unrelated tmux peer.
- `ask`/`notify_peer` from HTTP use the HTTP peer id/name as `from_peer` and respect circle rules.
- Dangerous tools (`spawn_peer`, `kill_peer`) enforce the chosen HTTP scope/role policy.

Smoke:

- Start daemon with HTTP MCP enabled on localhost and token configured.
- Use an MCP client that supports Streamable HTTP to connect to `http://127.0.0.1:8377/mcp` with bearer token.
- Run `tools/list`, `whoami`, `list_peers`, and a `notify_peer` to a test peer.
- Verify existing `repowire mcp` stdio still works for Claude/Codex/Gemini after setup.

## Docs impact

When implemented, update:

- README: optional remote MCP endpoint positioning and security warning.
- `docs/reference/mcp-tools.md`: note tools are shared by stdio and Streamable HTTP, with identity differences.
- `docs/reference/cli.md`: setup/status/config flags if added.
- `docs/agents/*`: keep stdio config as default; mention HTTP MCP only for clients that support remote MCP.
- `docs/surfaces/relay` or equivalent: explicitly state `/mcp` is not relay-exposed unless/ until scoped relay support ships.

## Open decisions

1. Should HTTP MCP expose all existing MCP tools initially, or start with read/routing only and gate lifecycle tools behind scopes?
2. Do we add a first-class `AgentType.MCP_HTTP`/backend value, or model it as a human/service surface without backend expansion?
3. Is the setup-generated `daemon.auth_token` sufficient for localhost-only v1, or should HTTP MCP require a new scoped token from day one?
4. What exact CLI should expose local config: `repowire mcp-url`, `repowire mcp config`, or status output?
5. Should HTTP MCP sessions be persisted as peers, or should they be ephemeral control-surface identities removed when the MCP session closes?
6. Should path and machine fields be redacted/empty for HTTP identities to avoid misleading list output?
