"""MCP server - thin HTTP client that delegates to daemon."""

from __future__ import annotations

import json
import os
from pathlib import Path
from uuid import uuid4

import httpx
from mcp.server.fastmcp import FastMCP

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def _detect_my_peer_name() -> str:
    """Detect current peer name from cwd folder name."""
    return Path.cwd().name


async def daemon_request(method: str, path: str, body: dict | None = None) -> dict:
    """Make an HTTP request to the daemon."""
    async with httpx.AsyncClient(timeout=300.0) as client:
        url = f"{DAEMON_URL}{path}"
        if method == "GET":
            resp = await client.get(url)
        else:
            resp = await client.post(url, json=body or {})
        resp.raise_for_status()
        return resp.json()


def create_mcp_server() -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire")

    @mcp.tool()
    async def list_peers() -> str:
        """List all registered peers in the mesh.

        Returns a list of peers with their name, path, machine, and status.
        """
        result = await daemon_request("GET", "/peers")
        return json.dumps(result.get("peers", []), indent=2)

    @mcp.tool()
    async def ask_peer(peer_name: str, query: str) -> str:
        """Ask a peer a question and wait for their response.

        Args:
            peer_name: Name of the peer to ask (e.g., "backend", "frontend")
            query: The question or request to send

        Returns:
            The peer's response text
        """
        my_name = _detect_my_peer_name()
        result = await daemon_request(
            "POST",
            "/query",
            {
                "from_peer": my_name,
                "to_peer": peer_name,
                "text": query,
            },
        )
        if result.get("error"):
            raise Exception(result["error"])
        return result.get("text", "")

    @mcp.tool()
    async def notify_peer(peer_name: str, message: str) -> str:
        """Send an async notification to a peer (fire-and-forget).

        Use for status updates, announcements, or replying to notifications.

        Args:
            peer_name: Name of the peer to notify
            message: The notification message

        Returns:
            Correlation ID (format: notif-XXXXXXXX) for tracking.
        """
        my_name = _detect_my_peer_name()
        correlation_id = f"notif-{uuid4().hex[:8]}"
        await daemon_request(
            "POST",
            "/notify",
            {
                "from_peer": my_name,
                "to_peer": peer_name,
                "text": f"[#{correlation_id}] {message}",
            },
        )
        return correlation_id

    @mcp.tool()
    async def broadcast(message: str) -> str:
        """Send a message to all online peers.

        Use for announcements that affect everyone, like deployment updates
        or breaking changes. Do NOT use for responses to queries.

        Args:
            message: The message to broadcast

        Returns:
            Confirmation message
        """
        my_name = _detect_my_peer_name()
        result = await daemon_request(
            "POST",
            "/broadcast",
            {
                "from_peer": my_name,
                "text": message,
            },
        )
        sent_to = result.get("sent_to", [])
        return f"Broadcast sent to: {', '.join(sent_to) if sent_to else 'no peers online'}"

    @mcp.tool()
    async def whoami() -> str:
        """Return information about yourself (the calling peer).

        Returns your peer name, circle, status, path, and metadata.
        Useful to understand your identity in the mesh without parsing list_peers.
        """
        my_name = _detect_my_peer_name()
        try:
            result = await daemon_request("GET", f"/peers/{my_name}")
            return json.dumps(
                {
                    "name": result.get("name"),
                    "circle": result.get("circle"),
                    "status": result.get("status"),
                    "path": result.get("path"),
                    "machine": result.get("machine"),
                    "metadata": result.get("metadata", {}),
                },
                indent=2,
            )
        except httpx.HTTPStatusError:
            return json.dumps({"name": my_name, "error": "Not registered"})

    @mcp.tool()
    async def set_circle(circle: str) -> str:
        """Join a named circle to communicate with peers in that circle.

        Use this to communicate with peers from different backends (e.g., OpenCode
        sessions). By default, claudemux peers are in a circle named after their
        tmux session, and OpenCode peers are in the "global" circle.

        Args:
            circle: Circle name to join (e.g., "dev", "global", "frontend")

        Returns:
            Confirmation message
        """
        my_name = _detect_my_peer_name()
        await daemon_request(
            "POST",
            "/peers/circle",
            {
                "peer_name": my_name,
                "circle": circle,
            },
        )
        return f"Joined circle: {circle}"

    return mcp


async def run_mcp_server() -> None:
    """Run the MCP server."""
    mcp = create_mcp_server()
    await mcp.run_stdio_async()
