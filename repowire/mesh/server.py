"""Repowire MCP Server - Happy++ for Claude session communication."""

from __future__ import annotations

import asyncio
import subprocess
from typing import Literal

from mcp.server.fastmcp import FastMCP

from repowire.transport.happy import HappyTransport

# Permission mode type
PermissionMode = Literal["default", "plan", "yolo", "bypassPermissions", "acceptEdits", "read-only", "safe-yolo"]

# Global transport instance
_transport: HappyTransport | None = None

def get_transport() -> HappyTransport:
    global _transport
    if _transport is None:
        _transport = HappyTransport()
    return _transport

def create_mcp_server() -> FastMCP:
    mcp = FastMCP("repowire")

    @mcp.tool()
    async def list_sessions() -> list[dict]:
        """List all Happy CLI sessions with metadata."""
        transport = get_transport()
        return await transport.list_sessions()

    @mcp.tool()
    async def send_message(
        session_id: str,
        text: str,
        permission_mode: PermissionMode = "default"
    ) -> str:
        """Send message to a Happy session and wait for response.

        Args:
            session_id: The session ID to send to
            text: The message text
            permission_mode: Permission mode (default, plan, yolo, etc.)
        """
        transport = get_transport()
        return await transport.send_message(session_id, text, permission_mode)

    @mcp.tool()
    async def create_session(path: str) -> dict:
        """Spawn a new Happy CLI session at the given path.

        Blocks until the session appears in list_sessions.

        Args:
            path: Directory path for the new session
        """
        transport = get_transport()

        # Get current sessions to compare
        before = {s["id"] for s in await transport.list_sessions()}

        # Spawn happy process
        subprocess.Popen(
            ["happy"],
            cwd=path,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Poll for new session (max 30 seconds)
        for _ in range(30):
            await asyncio.sleep(1)
            sessions = await transport.list_sessions()
            for session in sessions:
                if session["id"] not in before and session.get("path") == path:
                    return session

        raise TimeoutError(f"Session at {path} did not appear within 30 seconds")

    return mcp

async def run_mcp_server(stdio: bool = True) -> None:
    """Run the MCP server."""
    mcp = create_mcp_server()
    if stdio:
        await mcp.run_stdio_async()
    else:
        # HTTP mode if needed
        await mcp.run_async()
