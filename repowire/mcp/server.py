"""MCP server - thin HTTP client that delegates to daemon."""

from __future__ import annotations

import os
from pathlib import Path
from uuid import uuid4

import httpx
from mcp.server.fastmcp import FastMCP

from repowire.hooks._tmux import get_pane_id

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")

# Cached: peer identity is stable for the lifetime of this MCP process
_my_peer_name: str = Path.cwd().name


async def daemon_request(method: str, path: str, body: dict | None = None) -> dict:
    """Make an HTTP request to the daemon."""
    try:
        async with httpx.AsyncClient(timeout=300.0) as client:
            url = f"{DAEMON_URL}{path}"
            if method == "GET":
                resp = await client.get(url)
            else:
                resp = await client.post(url, json=body or {})
            resp.raise_for_status()
            return resp.json()
    except httpx.ConnectError:
        raise Exception("Repowire daemon is not reachable. Start it with 'repowire serve'.")
    except httpx.HTTPStatusError as e:
        raise Exception(f"Daemon error {e.response.status_code}: {e.response.text}")
    except httpx.TimeoutException:
        raise Exception("Daemon request timed out.")


async def _get_my_peer_name() -> str:
    """Get own peer name, preferring pane-based lookup."""
    pane_id = get_pane_id()
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{pane_id}")
            return result.get("peer_id") or result.get("display_name") or _my_peer_name
        except Exception:
            pass
    return _my_peer_name


def create_mcp_server() -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire")

    @mcp.tool()
    async def list_peers() -> str:
        """List all registered peers in the mesh.

        Returns TSV with columns: peer_id, name, project, circle, status, path
        """
        result = await daemon_request("GET", "/peers")
        peers = result.get("peers", [])
        rows = ["peer_id\tname\tproject\tcircle\tstatus\tpath"]
        for p in peers:
            project = p.get("metadata", {}).get("project", "") or ""
            rows.append(
                "\t".join(
                    [
                        p.get("peer_id", ""),
                        p.get("display_name") or p.get("name", ""),
                        project,
                        p.get("circle", ""),
                        p.get("status", ""),
                        p.get("path") or "",
                    ]
                )
            )
        return "\n".join(rows)

    @mcp.tool()
    async def ask_peer(peer_name: str, query: str, circle: str | None = None) -> str:
        """Ask a peer a question and wait for their response.

        For complex questions that may take a long time, consider using
        notify_peer instead — the peer can notify you back when ready.

        Args:
            peer_name: Name of the peer to ask (e.g., "backend", "frontend")
            query: The question or request to send
            circle: Circle to scope the lookup (optional — required when multiple
                    peers share the same name in different circles)

        Returns:
            The peer's response text
        """
        from_peer = await _get_my_peer_name()
        body: dict = {
            "from_peer": from_peer,
            "to_peer": peer_name,
            "text": query,
        }
        if circle is not None:
            body["circle"] = circle
        result = await daemon_request("POST", "/query", body)
        if result.get("error"):
            raise Exception(result["error"])
        return result.get("text", "")

    @mcp.tool()
    async def notify_peer(peer_name: str, message: str, circle: str | None = None) -> str:
        """Send an async notification to a peer (fire-and-forget).

        Use for status updates, announcements, or replying to notifications.

        Args:
            peer_name: Name of the peer to notify
            message: The notification message
            circle: Circle to scope the lookup (optional — required when multiple
                    peers share the same name in different circles)

        Returns:
            Correlation ID (format: notif-XXXXXXXX) for tracking.
        """
        from_peer = await _get_my_peer_name()
        correlation_id = f"notif-{uuid4().hex[:8]}"
        body: dict = {
            "from_peer": from_peer,
            "to_peer": peer_name,
            "text": f"[#{correlation_id}] {message}",
        }
        if circle is not None:
            body["circle"] = circle
        await daemon_request("POST", "/notify", body)
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
        from_peer = await _get_my_peer_name()
        result = await daemon_request(
            "POST",
            "/broadcast",
            {
                "from_peer": from_peer,
                "text": message,
            },
        )
        sent_to = result.get("sent_to", [])
        return f"Broadcast sent to: {', '.join(sent_to) if sent_to else 'no peers online'}"

    def _format_peer_tsv(result: dict) -> str:
        """Format a peer result dict as a TSV row with header."""
        project = result.get("metadata", {}).get("project", "") or ""
        header = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine"
        row = "\t".join(
            [
                result.get("peer_id", ""),
                result.get("display_name") or result.get("name", ""),
                project,
                result.get("circle", ""),
                result.get("status", ""),
                result.get("path") or "",
                result.get("machine") or "",
            ]
        )
        return f"{header}\n{row}"

    @mcp.tool()
    async def whoami() -> str:
        """Return information about yourself (the calling peer).

        Returns TSV with columns: peer_id, name, project, circle, status, path, machine
        """
        pane_id = get_pane_id()
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{pane_id}")
                return _format_peer_tsv(result)
            except Exception:
                pass  # fall through to fallback

        try:
            result = await daemon_request("GET", f"/peers/{_my_peer_name}")
            return _format_peer_tsv(result)
        except Exception as e:
            return (
                "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\n"
                f"\t{_my_peer_name}\t\t\tERROR: {e}\t\t"
            )

    @mcp.tool()
    async def spawn_peer(path: str, command: str, circle: str = "default") -> str:
        """Spawn a new coding session for a project.

        The command must exactly match an entry in daemon.spawn.allowed_commands
        in ~/.repowire/config.yaml. If no allowed_commands are configured, spawn
        is disabled and this will return an error.

        The spawned agent self-registers into the mesh via its SessionStart hook
        once it starts — no manual registration needed.

        Args:
            path: Absolute path to the project directory
            command: Command to run (e.g. "claude", "claude --dangerously-skip-permissions")
            circle: Circle to spawn into (default: "default")

        Returns:
            tmux_session reference (e.g. "default:myproject") — pass this to kill_peer to stop it
        """
        result = await daemon_request(
            "POST",
            "/spawn",
            {"path": path, "command": command, "circle": circle},
        )
        return result["tmux_session"]

    @mcp.tool()
    async def kill_peer(tmux_session: str) -> str:
        """Kill a spawned coding session.

        Args:
            tmux_session: Session reference returned by spawn_peer (e.g. "default:myproject")

        Returns:
            Confirmation message
        """
        await daemon_request("POST", "/kill", {"tmux_session": tmux_session})
        return f"Killed {tmux_session}"

    return mcp


async def run_mcp_server() -> None:
    """Run the MCP server."""
    mcp = create_mcp_server()
    await mcp.run_stdio_async()
