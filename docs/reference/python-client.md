# Python client

`repowire.client.AsyncRepowireClient` is the supported way to talk to the daemon from Python code that is not an agent. It wraps the daemon's HTTP API in a typed async surface using pydantic models for every response. Apps and scripts should depend on this rather than reach into daemon internals.

## Construction

Pass a base URL and optional auth token. The default targets a local daemon at `127.0.0.1:8377`. The client is async-context-managed; `aclose()` is wired into `__aexit__`.

```python
from repowire.client import AsyncRepowireClient

async with AsyncRepowireClient() as client:
    health = await client.health()
    print(health.version)

# with auth and a custom base
async with AsyncRepowireClient(
    "https://repowire.io",
    auth_token="rw_...",
    timeout=10.0,
) as client:
    peers = await client.list_peers(status="online")
```

You can also inject your own `httpx.AsyncClient` via the `client=` kwarg, in which case ownership stays with you and `aclose()` becomes a no-op.

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

## Listing and inspection

Pull current mesh state. `list_peers` accepts daemon-supported filters; `get_peer` resolves a single peer by name or id. `pending_asks` returns open asks targeting one pane or peer.

```python
for peer in await client.list_peers(status="online"):
    print(peer.name, peer.circle, peer.description)

peer = await client.get_peer("project-b")

asks = await client.pending_asks(peer_id=peer.peer_id)
```

## Spawning and lifecycle

`spawn` launches a new agent session subject to `daemon.spawn.allowed_commands`. `spawn_config` reports what the daemon will accept without attempting a spawn. `kill_peer` terminates a peer cleanly.

```python
info = await client.spawn_config()
if "claude" in info.allowed_commands:
    spawn = await client.spawn(
        path="/home/me/projects/project-c",
        command="claude",
        circle="docs",
        message="help me draft a reference page",
    )
    print(spawn.display_name, spawn.tmux_session)
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
