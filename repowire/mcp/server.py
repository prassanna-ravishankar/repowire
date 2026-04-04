"""MCP server - thin HTTP client that delegates to daemon."""

from __future__ import annotations

import logging
import os
from pathlib import Path
from urllib.parse import quote
from uuid import uuid4

import httpx
from mcp.server.fastmcp import FastMCP

from repowire.config.models import DEFAULT_DAEMON_URL
from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import get_display_name
from repowire.protocol.errors import DaemonConnectionError, DaemonHTTPError, DaemonTimeoutError

logger = logging.getLogger(__name__)

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

# Lazy singleton HTTP client — reused across all daemon requests
_http_client: httpx.AsyncClient | None = None

# Cached peer name: resolved lazily from env var, pane lookup, or registration
_cached_peer_name: str | None = None

# Lazy registration: ensure peer is registered on first MCP tool use
_registered: bool = False


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
        raise DaemonConnectionError()
    except httpx.HTTPStatusError as e:
        raise DaemonHTTPError(e.response.status_code, e.response.text)
    except httpx.TimeoutException:
        raise DaemonTimeoutError()


async def _get_my_peer_name() -> str:
    """Get own peer name. Cached after first resolution.

    Priority: REPOWIRE_DISPLAY_NAME env var > pane-based daemon lookup > cwd folder name.
    """
    global _cached_peer_name
    if _cached_peer_name is not None:
        return _cached_peer_name
    # Pane-based lookup is most authoritative (handles suffix collisions)
    pane_id = get_pane_id()
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
            name = result.get("display_name") or result.get("peer_id")
            if name:
                _cached_peer_name = name
                return name
        except Exception:
            pass
    # Fall back to env var (set by session handler) or cwd folder name
    _cached_peer_name = get_display_name()
    return _cached_peer_name


async def _ensure_registered() -> None:
    """Lazy-register this peer with the daemon on first MCP tool use.

    Skips registration if the peer already exists (e.g. SessionStart hook
    already registered it). Only registers as fallback for agents where
    hooks don't fire (one-shot prompt mode).
    """
    global _registered, _cached_peer_name
    if _registered:
        return
    _registered = True
    name = await _get_my_peer_name()
    try:
        await daemon_request("GET", f"/peers/{name}")
        return  # Already registered -- skip
    except Exception:
        pass
    # Detect backend from env set by each agent runtime
    if os.environ.get("GEMINI_CLI"):
        backend = "gemini"
    elif ".codex/" in os.environ.get("PATH", ""):
        backend = "codex"
    else:
        backend = os.environ.get("REPOWIRE_BACKEND", "claude-code")
    try:
        result = await daemon_request("POST", "/peers", {
            "name": Path.cwd().name,
            "path": str(Path.cwd()),
            "circle": "default",
            "backend": backend,
        })
        # Cache the daemon-assigned name
        assigned = result.get("display_name")
        if assigned:
            _cached_peer_name = assigned
    except Exception:
        pass  # Best-effort -- daemon may be down


def create_mcp_server() -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire")

    tsv_header = "peer_id\tname\tproject\tcircle\trole\tstatus\tpath\tmachine\tdescription\tbackend"

    def _peer_to_tsv_row(p: dict) -> str:
        """Format a single peer dict as a TSV row."""
        project = p.get("metadata", {}).get("project", "") or ""
        return "\t".join(
            [
                p.get("peer_id", ""),
                p.get("display_name") or p.get("name", ""),
                project,
                p.get("circle", ""),
                p.get("role", "agent"),
                p.get("status", ""),
                p.get("path") or "",
                p.get("machine") or "",
                p.get("description") or "",
                p.get("backend", ""),
            ]
        )

    @mcp.tool()
    async def list_peers() -> str:
        """[Repowire mesh] List all peers across projects and machines.

        Returns TSV: peer_id, name, project, circle, status, path, machine, description.
        These are cross-project peers reachable via ask_peer/notify_peer. Do NOT
        use SendMessage to contact them -- SendMessage is a Claude Code harness
        tool for same-session teammates only.
        """
        await _ensure_registered()
        result = await daemon_request("GET", "/peers")
        peers = result.get("peers", [])
        rows = [tsv_header]
        for p in peers:
            rows.append(_peer_to_tsv_row(p))
        return "\n".join(rows)

    @mcp.tool()
    async def ask_peer(peer_name: str, query: str, circle: str | None = None) -> str:
        """[Repowire mesh] Ask a peer in another project and wait for response.

        Reaches peers across different projects and machines via the repowire
        daemon. For complex questions that may take a long time, consider using
        notify_peer instead.

        Do NOT use SendMessage to reach repowire peers. SendMessage is a Claude
        Code harness tool for same-session teammates only.

        Args:
            peer_name: Name of the peer to ask (e.g., "backend", "frontend")
            query: The question or request to send
            circle: Circle to scope the lookup (optional, required when multiple
                    peers share the same name in different circles)

        Returns:
            The peer's response text
        """
        await _ensure_registered()
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
        """[Repowire mesh] Send a fire-and-forget notification to a peer in another project.

        Use for status updates, announcements, or replying to notifications.
        Special peers: 'telegram' sends to user's phone.
        The dashboard sees your responses automatically via chat turns - no need to notify it.

        Do NOT use SendMessage to reach repowire peers. SendMessage is a Claude
        Code harness tool for same-session teammates only.

        Args:
            peer_name: Name of the peer to notify
            message: The notification message
            circle: Circle to scope the lookup (optional, required when multiple
                    peers share the same name in different circles)

        Returns:
            Correlation ID (format: notif-XXXXXXXX) for tracking.
        """
        await _ensure_registered()
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
        """[Repowire mesh] Broadcast to all online peers across the mesh.

        Use for announcements that affect everyone, like deployment updates
        or breaking changes. Do NOT use for responses to queries.

        Do NOT use SendMessage to reach repowire peers. SendMessage is a Claude
        Code harness tool for same-session teammates only.

        Args:
            message: The message to broadcast

        Returns:
            Confirmation message
        """
        await _ensure_registered()
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
        """[Repowire mesh] Return your identity in the repowire mesh.

        Returns TSV with columns: peer_id, name, project, circle, status, path, machine, description
        """
        await _ensure_registered()
        pane_id = get_pane_id()
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
                return _format_peer_tsv(result)
            except Exception:
                pass  # fall through to fallback

        name = await _get_my_peer_name()
        try:
            result = await daemon_request("GET", f"/peers/{name}")
            return _format_peer_tsv(result)
        except Exception as e:
            return f"{tsv_header}\n\t{name}\t\t\tERROR: {e}\t\t\t"

    @mcp.tool()
    async def set_description(description: str) -> str:
        """[Repowire mesh] Update your task description, visible to other peers via list_peers.

        Call this at the start of a task so peers know what you're working on.

        Args:
            description: Short description of your current task (e.g., "fixing auth bug")

        Returns:
            Confirmation message
        """
        await _ensure_registered()
        pane_id = get_pane_id()
        name = ""
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
                name = result.get("display_name") or result.get("name", "")
            except Exception as e:
                logger.warning("Could not get peer name by pane_id '%s': %s", pane_id, e)
        if not name:
            name = await _get_my_peer_name()
        await daemon_request("POST", f"/peers/{name}/description", {"description": description})
        return f"description updated: {description}"

    @mcp.tool()
    async def spawn_peer(path: str, command: str, circle: str = "default") -> str:
        """[Repowire mesh] Spawn a new coding session in a different project directory.

        The command must exactly match an entry in daemon.spawn.allowed_commands
        in ~/.repowire/config.yaml. If no allowed_commands are configured, spawn
        is disabled and this will return an error.

        The spawned agent self-registers into the mesh via its SessionStart hook
        within a few seconds. Use list_peers() to confirm registration and get
        the peer_id.

        The circle maps to the tmux session name and cannot be reassigned after
        spawn.

        Do NOT use SendMessage to reach spawned peers. SendMessage is a Claude
        Code harness tool for same-session teammates only. Use ask_peer() or
        notify_peer() instead.

        Args:
            path: Absolute path to the project directory
            command: Command to run (e.g. "claude", "claude --dangerously-skip-permissions")
            circle: Circle to spawn into (default: "default") -- maps to tmux session name

        Returns:
            Spawn confirmation with display_name and tmux_session
        """
        result = await daemon_request(
            "POST",
            "/spawn",
            {"path": path, "command": command, "circle": circle},
        )
        name = result["display_name"]
        tmux = result["tmux_session"]
        return (
            f"Spawned {name} (tmux: {tmux}). "
            f"Peer will self-register shortly. Use list_peers() to confirm "
            f"and get peer_id. Address it as '{name}' via ask_peer/notify_peer."
        )

    @mcp.tool()
    async def kill_peer(tmux_session: str) -> str:
        """[Repowire mesh] Kill a spawned coding session.

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
