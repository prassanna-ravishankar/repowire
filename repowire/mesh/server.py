from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

from mcp.server.fastmcp import FastMCP

from repowire.auth.happy import load_credentials
from repowire.mesh.peers import Peer, PeerRegistry
from repowire.mesh.state import SharedState
from repowire.transport.base import Transport
from repowire.transport.happy import HappyTransport
from repowire.transport.opencode import OpenCodeTransport

registry = PeerRegistry()
state = SharedState()

_opencode_transport: OpenCodeTransport | None = None
_happy_transport: HappyTransport | None = None


def get_opencode_transport() -> OpenCodeTransport:
    global _opencode_transport
    if _opencode_transport is None:
        _opencode_transport = OpenCodeTransport()
    return _opencode_transport


def get_happy_transport() -> HappyTransport:
    global _happy_transport
    if _happy_transport is None:
        creds = load_credentials()
        if not creds:
            raise ValueError(
                "Happy credentials not found. Run 'repowire auth happy' first."
            )
        _happy_transport = HappyTransport(creds)
    return _happy_transport


def get_transport_for_peer(peer: Peer) -> Transport:
    if peer.agent_type == "opencode":
        return get_opencode_transport()
    elif peer.agent_type == "happy":
        return get_happy_transport()
    raise ValueError(f"Unsupported agent type: {peer.agent_type}")


def create_mesh_server(port: int = 9876) -> FastMCP:
    mcp = FastMCP("repowire-mesh", json_response=True, host="127.0.0.1", port=port)

    @mcp.tool()
    async def register(
        name: str,
        agent_type: str,
        session_id: str,
        path: str,
        capabilities: list[str] | None = None,
    ) -> dict[str, Any]:
        """Register this session as a peer in the mesh."""
        peer = Peer(
            name=name,
            agent_type=agent_type,  # type: ignore
            session_id=session_id,
            path=path,
            capabilities=capabilities or [],
        )
        peers = registry.register(peer)
        return {
            "success": True,
            "peers": [
                {"name": p.name, "agent_type": p.agent_type, "path": p.path}
                for p in peers
            ],
        }

    @mcp.tool()
    async def unregister(name: str) -> dict[str, Any]:
        """Remove this session from the mesh."""
        success = registry.unregister(name)
        return {"success": success}

    @mcp.tool()
    async def list_peers() -> dict[str, Any]:
        """List all registered peers in the mesh."""
        peers = registry.list_all()
        return {
            "peers": [
                {
                    "name": p.name,
                    "agent_type": p.agent_type,
                    "path": p.path,
                    "capabilities": p.capabilities,
                    "is_active": p.is_active,
                }
                for p in peers
            ]
        }

    @mcp.tool()
    async def write_state(key: str, value: str) -> dict[str, Any]:
        """Write a key-value pair to shared state."""
        await state.write(key, value)
        return {"success": True, "key": key}

    @mcp.tool()
    async def read_state(key: str | None = None) -> dict[str, Any]:
        """Read from shared state. If key is None, returns all state."""
        value = await state.read(key)
        return {"value": value}

    @mcp.tool()
    async def read_peer_file(target: str, file_path: str) -> dict[str, Any]:
        """Read a file from a peer's working directory."""
        peer = registry.get(target)
        if not peer:
            return {"error": f"Peer '{target}' not found", "success": False}

        full_path = Path(peer.path) / file_path

        try:
            resolved = full_path.resolve()
            peer_path = Path(peer.path).resolve()
            if not str(resolved).startswith(str(peer_path)):
                return {"error": "Path traversal not allowed", "success": False}
        except Exception:
            return {"error": "Invalid path", "success": False}

        if not resolved.exists():
            return {"error": f"File not found: {file_path}", "success": False}

        try:
            content = resolved.read_text()
            if len(content) > 100000:
                content = content[:100000] + "\n... (truncated)"
            return {"content": content, "success": True}
        except Exception as e:
            return {"error": str(e), "success": False}

    @mcp.tool()
    async def ask_peer(
        caller: str,
        target: str,
        query: str,
        timeout: float = 60.0,
    ) -> dict[str, Any]:
        """Send a query to another peer and wait for response."""
        peer = registry.get(target)
        if not peer:
            return {"error": f"Peer '{target}' not found", "success": False}

        if target == caller:
            return {"error": "Cannot query yourself", "success": False}

        formatted_query = f"[PEER QUERY from {caller}]: {query}"

        try:
            transport = get_transport_for_peer(peer)
            response = await asyncio.wait_for(
                transport.send_message(peer.session_id, formatted_query),
                timeout=timeout,
            )
            return {"response": response, "success": True}
        except asyncio.TimeoutError:
            return {
                "error": f"Peer '{target}' did not respond within {timeout}s",
                "success": False,
            }
        except ValueError as e:
            return {"error": str(e), "success": False}
        except Exception as e:
            return {"error": f"Transport error: {e}", "success": False}

    @mcp.tool()
    async def notify_peer(caller: str, target: str, message: str) -> dict[str, Any]:
        """Send a notification to a peer (fire-and-forget)."""
        peer = registry.get(target)
        if not peer:
            return {"error": f"Peer '{target}' not found", "success": False}

        formatted = f"[NOTIFICATION from {caller}]: {message}"

        try:
            transport = get_transport_for_peer(peer)
            await transport.send_notification(peer.session_id, formatted)
            return {"success": True}
        except ValueError as e:
            return {"error": str(e), "success": False}
        except Exception as e:
            return {"error": f"Transport error: {e}", "success": False}

    @mcp.tool()
    async def broadcast(caller: str, message: str) -> dict[str, Any]:
        """Send a notification to all peers."""
        notified: list[str] = []
        errors: list[str] = []

        formatted = f"[BROADCAST from {caller}]: {message}"

        for peer in registry.list_all():
            if peer.name == caller:
                continue

            try:
                transport = get_transport_for_peer(peer)
                await transport.send_notification(peer.session_id, formatted)
                notified.append(peer.name)
            except Exception as e:
                errors.append(f"{peer.name}: {e}")

        return {"notified": notified, "errors": errors, "success": len(errors) == 0}

    return mcp


def run_mesh_server(port: int = 9876, stdio: bool = False) -> None:
    mcp = create_mesh_server(port=port)

    if stdio:
        mcp.run(transport="stdio")
    else:
        mcp.run(transport="streamable-http")
