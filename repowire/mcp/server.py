from __future__ import annotations

import asyncio

from mcp.server.fastmcp import FastMCP

from repowire.session.manager import TmuxSessionManager

_manager: TmuxSessionManager | None = None


def get_manager() -> TmuxSessionManager:
    global _manager
    if _manager is None:
        _manager = TmuxSessionManager()
    return _manager


def create_mcp_server() -> FastMCP:
    mcp = FastMCP("repowire")
    manager = get_manager()

    @mcp.tool()
    async def list_peers() -> list[dict]:
        """List all registered peers in the mesh.

        Returns a list of peers with their name, path, machine, and status.
        """
        return [p.to_dict() for p in manager.list_peers()]

    @mcp.tool()
    async def ask_peer(peer_name: str, query: str) -> str:
        """Ask a peer a question and wait for their response.

        Args:
            peer_name: Name of the peer to ask (e.g., "backend", "frontend")
            query: The question or request to send

        Returns:
            The peer's response text
        """
        return await manager.send_query(peer_name, query)

    @mcp.tool()
    async def notify_peer(peer_name: str, message: str) -> str:
        """Send a notification to a peer (fire-and-forget).

        Args:
            peer_name: Name of the peer to notify
            message: The notification message

        Returns:
            Confirmation message
        """
        await manager.send_notification(peer_name, message)
        return f"Notification sent to {peer_name}"

    @mcp.tool()
    async def broadcast(message: str) -> str:
        """Send a message to all online peers.

        Args:
            message: The message to broadcast

        Returns:
            Confirmation message
        """
        await manager.broadcast(message)
        peers = [p.name for p in manager.list_peers() if p.status.value == "online"]
        return f"Broadcast sent to: {', '.join(peers) if peers else 'no peers online'}"

    @mcp.tool()
    async def register_peer(name: str, tmux_session: str, path: str) -> str:
        """Register a new peer for communication.

        Args:
            name: Human-readable name for the peer
            tmux_session: Name of the tmux session running Claude
            path: Working directory path

        Returns:
            Confirmation message
        """
        manager.config.add_peer(name, tmux_session, path)
        return f"Peer '{name}' registered (tmux: {tmux_session}, path: {path})"

    return mcp


async def run_mcp_server() -> None:
    manager = get_manager()
    await manager.start()

    try:
        mcp = create_mcp_server()
        await mcp.run_stdio_async()
    finally:
        await manager.stop()
