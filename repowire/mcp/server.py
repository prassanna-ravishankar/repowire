"""MCP server - thin HTTP client that delegates to daemon."""

from __future__ import annotations

import logging
import os
from pathlib import Path
from uuid import uuid4

import httpx
from mcp.server.fastmcp import FastMCP

from repowire.config.models import DEFAULT_DAEMON_URL
from repowire.hooks._tmux import get_pane_id

logger = logging.getLogger(__name__)

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

# Cached: peer identity is stable for the lifetime of this MCP process
_my_peer_name: str = Path.cwd().name

# Lazy singleton HTTP client — reused across all daemon requests
_http_client: httpx.AsyncClient | None = None

# Cached peer name from pane-based lookup (stable for MCP process lifetime)
_cached_peer_name: str | None = None


def _get_http_client() -> httpx.AsyncClient:
    global _http_client
    if _http_client is None:
        _http_client = httpx.AsyncClient(timeout=300.0)
    return _http_client


async def daemon_request(method: str, path: str, body: dict | None = None) -> dict:
    """Make an HTTP request to the daemon."""
    global _http_client
    try:
        client = _get_http_client()
        url = f"{DAEMON_URL}{path}"
        if method == "GET":
            resp = await client.get(url)
        else:
            resp = await client.post(url, json=body or {})
        resp.raise_for_status()
        return resp.json()
    except httpx.ConnectError:
        _http_client = None  # Reset stale client so next call reconnects
        raise Exception("Repowire daemon is not reachable. Start it with 'repowire serve'.")
    except httpx.HTTPStatusError as e:
        raise Exception(f"Daemon error {e.response.status_code}: {e.response.text}")
    except httpx.TimeoutException:
        raise Exception("Daemon request timed out.")


async def _get_my_peer_name() -> str:
    """Get own peer name, preferring pane-based lookup. Cached after first resolution."""
    global _cached_peer_name
    if _cached_peer_name is not None:
        return _cached_peer_name
    pane_id = get_pane_id()
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{pane_id}")
            name = result.get("display_name") or result.get("peer_id") or _my_peer_name
            _cached_peer_name = name
            return name
        except Exception:
            pass
    _cached_peer_name = _my_peer_name
    return _my_peer_name


def create_mcp_server() -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire")

    tsv_header = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription"

    def _peer_to_tsv_row(p: dict) -> str:
        """Format a single peer dict as a TSV row (8 columns)."""
        project = p.get("metadata", {}).get("project", "") or ""
        return "\t".join(
            [
                p.get("peer_id", ""),
                p.get("display_name") or p.get("name", ""),
                project,
                p.get("circle", ""),
                p.get("status", ""),
                p.get("path") or "",
                p.get("machine") or "",
                p.get("description") or "",
            ]
        )

    @mcp.tool()
    async def list_peers() -> str:
        """List all registered peers in the mesh.

        Returns TSV with columns: peer_id, name, project, circle, status, path, machine, description
        """
        result = await daemon_request("GET", "/peers")
        peers = result.get("peers", [])
        rows = [tsv_header]
        for p in peers:
            rows.append(_peer_to_tsv_row(p))
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
        return f"{tsv_header}\n{_peer_to_tsv_row(result)}"

    @mcp.tool()
    async def whoami() -> str:
        """Return information about yourself (the calling peer).

        Returns TSV with columns: peer_id, name, project, circle, status, path, machine, description
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
            return f"{tsv_header}\n\t{_my_peer_name}\t\t\tERROR: {e}\t\t\t"

    @mcp.tool()
    async def set_description(description: str) -> str:
        """Update your task description, visible to other peers via list_peers.

        Call this at the start of a task so peers know what you're working on.

        Args:
            description: Short description of your current task (e.g., "fixing auth bug")

        Returns:
            Confirmation message
        """
        pane_id = get_pane_id()
        name = ""
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{pane_id}")
                name = result.get("display_name") or result.get("name", "")
            except Exception as e:
                logger.warning("Could not get peer name by pane_id '%s': %s", pane_id, e)
        if not name:
            name = _my_peer_name
        await daemon_request("POST", f"/peers/{name}/description", {"description": description})
        return f"description updated: {description}"

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
