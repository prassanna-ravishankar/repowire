# Python client

`repowire.client.AsyncRepowireClient` is the supported way to talk to the daemon from Python code that is not an agent. It wraps the daemon's HTTP API in a typed async surface using pydantic models for every response. Apps and scripts should depend on this rather than reach into daemon internals.

## Construction

Pass a base URL and optional auth token. The default targets a local daemon at `127.0.0.1:8377`. The client is async-context-managed; `aclose()` is wired into `__aexit__`.

```python
from repowire.client import AsyncRepowireClient

async with AsyncRepowireClient() as client:
    health = await client.health()
    print(health.version)
    print(health.channel.status, health.acp_broker.status)

# with auth and a custom base
async with AsyncRepowireClient(
    "https://repowire.io",
    auth_token="rw_...",
    timeout=10.0,
) as client:
    peers = await client.list_peers(status="online")
```

You can also inject your own `httpx.AsyncClient` via the `client=` kwarg, in which case ownership stays with you and `aclose()` becomes a no-op.

`health.channel` and `health.acp_broker` are passive readiness snapshots. They
do not spawn ACP subprocesses or probe agents; they report daemon-known state
such as channel runtime/auth readiness, configured ACP peers, in-flight broker
prompts, permission-relay pending count, and the last recorded error.

## Identity

Every routing call takes a `from_peer` kwarg. Unlike MCP tools, the client does not auto-detect identity from the tmux pane. Pass the registered name explicitly. Register first if the caller is not already a peer:

```python
reg = await client.register_peer(
    name="my-script",
    path="/home/me/scripts",
    backend="python",
)
print(reg.peer_id, reg.display_name)
```

## Ask, ack, notify, broadcast

The four routing primitives mirror the MCP tool surface. Asks are non-blocking and return a `correlation_id`; the recipient closes them with `ack`. Reply content rides on the same ack call.

```python
result = await client.ask(
    to_peer="project-b",
    text="Which port is the daemon on?",
    from_peer="my-script",
)
cid = result.correlation_id

# elsewhere, on the recipient side or from an orchestrator:
await client.ack(cid, from_peer="project-b", message="8377")

await client.notify(
    to_peer="telegram",
    text="long task done",
    from_peer="my-script",
)

bc = await client.broadcast(
    "rebasing main",
    from_peer="my-script",
)
print(bc)
```

`ask`, `ack`, and `notify` also accept an optional `attachments=[...]` list using
daemon attachment metadata (`id`, `path`, `filename`, `size`, `content_type`).
Omit it for the text-only shape; existing callers do not need to change.

For old blocking integrations, `query(to_peer, text, ...)` is retained as a
compatibility helper. It returns the historical `{text, error, status}` model,
but the daemon now backs it with the same ask/answer lifecycle used by `ask`.

## Listing and inspection

Pull current mesh state. `list_peers` accepts daemon-supported filters; `get_peer` resolves a single peer by name or id. `pending_asks` returns open asks for one pane or peer; pass `direction="outbound"` or `direction="both"` to inspect asks opened by that peer.

```python
for peer in await client.list_peers(status="online"):
    print(peer.name, peer.circle, peer.description)

peer = await client.get_peer("project-b")

asks = await client.pending_asks(peer_id=peer.peer_id)
outbound = await client.pending_asks(peer_id=peer.peer_id, direction="outbound")
```

## Spawning and lifecycle

`spawn` launches a new agent session subject to `daemon.spawn.commands` and `daemon.spawn.allowed_paths`. `spawn_config` reports which backend launch profiles and optional model profiles are configured. Pass `profile` to append args from `daemon.spawn.profiles.<backend>.<profile>`. Peer records include optional `model`, the last observed runtime model reported by the backend; this is not inferred from the spawn profile. Omit `circle` to use the daemon default (`default`), or pass it explicitly for another circle. The result includes `registration_state`, optional `peer_id`, and `warnings`; Antigravity daemon spawn returns `registration_state="cli_fallback"` while upstream `agy` hooks are pending. `kill_peer` always removes the peer from the mesh registry. It kills the backing tmux pane only when the daemon can verify the target pane by spawn ownership or matching pane `peer_id` metadata; otherwise the response reports `tmux_killed=null`.

`restart_peer` resumes a peer on the same backend, path, circle, role, and mesh identity. It refuses cross-host peers, missing/stale backend resume ids, and unverified live panes. Manually attached peers can be restarted when their pane metadata proves the target `peer_id`; cwd/path match alone is not enough.

Restart is same-window/name first, not same-pane: tmux allocates a fresh pane through the normal spawn path after the old verified pane is killed. The response includes `resume_mode=resumed`; if no valid backend session is available, the API returns `409 resume_unavailable` before killing anything.

`resume_session` is the session-native equivalent for a detached durable
Repowire session id. It validates the captured backend runtime id against local
session storage, fails on active/stale/unsupported sessions, and can either
inspect (`dry_run=True`) or spawn the backend resume (`dry_run=False`).

```python
info = await client.spawn_config()
if "claude-code" in info.commands:
    spawn = await client.spawn(
        path="/home/me/projects/project-c",
        backend="claude-code",
        profile="fast",
        circle="docs",
        message="help me draft a reference page",
    )
    print(spawn.display_name, spawn.tmux_session, spawn.peer_id, spawn.registration_state)

restart = await client.restart_peer("project-c-claude-code", dry_run=True)
print(restart.status, restart.resume_mode)

resumed = await client.resume_session(
    "rw-session-abc123",
    dry_run=False,
    profile="fast",
    message="continue",
)
print(resumed.action, resumed.spawned_display_name)
```

## Errors

All methods raise one of three typed errors from `repowire.protocol.errors`:

- `DaemonConnectionError` — the daemon is not reachable (most often, not running).
- `DaemonTimeoutError` — the daemon accepted the connection but did not respond in time.
- `DaemonHTTPError(status, body)` — the daemon returned a non-2xx response.

## Stability

The client is the public Python surface; depend on it rather than the daemon HTTP routes, which may shift between releases. Repowire is pre-1.0, so method signatures and pydantic models may still adjust across minor versions. Additions are preferred over breaks, but explicit breaks will happen when the design wants them.

## See also

Agents call the same primitives through [MCP tools](mcp-tools.md). The semantics are identical; only the identity resolution differs.
