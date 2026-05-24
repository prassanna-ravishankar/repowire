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
from repowire.hooks._tmux import get_pane_id, get_tmux_info
from repowire.hooks.utils import get_display_name, read_pane_runtime_metadata
from repowire.protocol.errors import DaemonConnectionError, DaemonHTTPError, DaemonTimeoutError
from repowire.spawn_hints import consume_hint_full

logger = logging.getLogger(__name__)

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", DEFAULT_DAEMON_URL)

# Lazy singleton HTTP client — reused across all daemon requests
_http_client: httpx.AsyncClient | None = None

# Cached peer name: resolved lazily from env var, pane lookup, or registration
_cached_peer_name: str | None = None
_cached_peer_id: str | None = None

# Cached caller identity (circle + role), populated alongside _cached_peer_name.
# Used by list_peers/ask/notify_peer to scope to the caller's circle by default
# and to widen the default when the caller's role is orchestrator. Avoids
# re-hitting the daemon for whoami on every list_peers call.
_cached_my_circle: str | None = None
_cached_my_role: str | None = None

# Lazy registration: ensure peer is registered on first MCP tool use
_registered: bool = False

# HTTP MCP mode is configured only by the daemon-mounted Streamable HTTP app.
# Stdio MCP leaves these unset and keeps the existing tmux/cwd identity flow.
_http_mcp_mode: bool = False
_http_mcp_auth_token: str | None = None
_http_mcp_allow_dangerous_tools: bool = False
_http_mcp_identity: dict[str, str] = {
    "name": "mcp-http",
    "circle": "global",
    "role": "human",
}


_BYPASS_ROLES = {"service", "orchestrator", "human"}


def _cache_identity(result: dict) -> None:
    """Cache canonical identity fields returned by daemon peer endpoints."""
    global _cached_peer_id, _cached_peer_name, _cached_my_circle, _cached_my_role
    if result.get("peer_id"):
        _cached_peer_id = result["peer_id"]
    name = result.get("display_name") or result.get("peer_name") or result.get("name")
    if name:
        _cached_peer_name = name
    if result.get("circle"):
        _cached_my_circle = result.get("circle")
    if result.get("role"):
        _cached_my_role = result.get("role")


def _orchestrator_self_claim_allowed(peer: dict) -> bool:
    """Return whether this peer is eligible to reclaim the orchestrator role."""
    display_name = str(peer.get("display_name") or peer.get("name") or "").lower()
    path_name = Path(str(peer.get("path") or "")).name.lower()
    return display_name == "orchestrator" or path_name == "orchestrator"


async def _resolve_circle_for_send(
    peer_name: str, explicit_circle: str | None, my_circle: str | None
) -> str | None:
    """Resolve which circle to pass to /ask or /notify for `peer_name`.

    - Explicit circle wins.
    - Otherwise try caller's circle; if a peer is found there, use it.
    - Otherwise fall back to a global lookup, but only accept the match
      when its role bypasses circles (service/orchestrator/human).
    - Returns None to mean "let the daemon resolve globally without a
      circle hint" — the daemon's ambiguous-name refusal then applies.
    """
    if explicit_circle is not None:
        return explicit_circle
    if my_circle:
        try:
            await daemon_request(
                "GET",
                f"/peers/{quote(peer_name, safe='')}",
                params={"circle": my_circle},
            )
            return my_circle
        except DaemonHTTPError as e:
            if e.status != 404:
                raise
        try:
            result = await daemon_request(
                "GET", f"/peers/{quote(peer_name, safe='')}"
            )
        except DaemonHTTPError as e:
            if e.status == 404:
                return my_circle  # let daemon return its own 404 on the send
            raise
        role = (result.get("role") or "").lower()
        if role in _BYPASS_ROLES:
            return result.get("circle")
        return my_circle  # no bypass match; force daemon to 404 in caller's circle
    return None


async def _get_my_identity() -> tuple[str | None, str | None, str | None]:
    """Return (name, circle, role) for the calling peer.

    Cached after first resolve; safe to call from every MCP entry. Returns
    Nones when the daemon can't be reached or the peer hasn't registered yet.
    """
    global _cached_my_circle, _cached_my_role
    name = await _get_my_peer_name()
    if _cached_my_circle is not None and _cached_my_role is not None:
        return name, _cached_my_circle, _cached_my_role
    if not name:
        return name, _cached_my_circle, _cached_my_role
    try:
        identifier = _cached_peer_id or name
        result = await daemon_request("GET", f"/peers/{quote(identifier, safe='')}")
        _cache_identity(result)
        _cached_my_circle = result.get("circle") or _cached_my_circle
        _cached_my_role = result.get("role") or _cached_my_role
    except Exception:
        pass
    return name, _cached_my_circle, _cached_my_role


async def _get_my_peer_identifier() -> str:
    """Return the canonical peer_id when known, otherwise the display name."""
    await _get_my_identity()
    return _cached_peer_id or await _get_my_peer_name()


def reset_mcp_context() -> None:
    """Reset process-global MCP identity/cache to stdio defaults."""
    global _http_mcp_mode, _http_mcp_auth_token, _http_mcp_allow_dangerous_tools
    global _registered, _cached_peer_name, _cached_peer_id, _cached_my_circle, _cached_my_role
    _http_mcp_mode = False
    _http_mcp_auth_token = None
    _http_mcp_allow_dangerous_tools = False
    _registered = False
    _cached_peer_name = None
    _cached_peer_id = None
    _cached_my_circle = None
    _cached_my_role = None


def configure_http_mcp_context(
    *,
    auth_token: str | None,
    allow_dangerous_tools: bool = False,
    name: str = "mcp-http",
    circle: str = "global",
    role: str = "human",
) -> None:
    """Configure daemon-mounted HTTP MCP identity and internal daemon auth."""
    global _http_mcp_mode, _http_mcp_auth_token, _http_mcp_allow_dangerous_tools
    global _http_mcp_identity, _registered, _cached_peer_name, _cached_peer_id
    global _cached_my_circle, _cached_my_role
    _http_mcp_mode = True
    _http_mcp_auth_token = auth_token
    _http_mcp_allow_dangerous_tools = allow_dangerous_tools
    _http_mcp_identity = {"name": name, "circle": circle, "role": role}
    # The daemon may be rebuilt in tests or restarted in-process; do not carry
    # stdio/cwd identity into the HTTP surface.
    _registered = False
    _cached_peer_name = None
    _cached_peer_id = None
    _cached_my_circle = None
    _cached_my_role = None


def _get_http_client() -> httpx.AsyncClient:
    global _http_client
    if _http_client is None:
        _http_client = httpx.AsyncClient(timeout=300.0)
    return _http_client


async def daemon_request(
    method: str, path: str, body: dict | None = None, params: dict | None = None
) -> dict:
    """Make an HTTP request to the daemon."""
    global _http_client
    try:
        client = _get_http_client()
        url = f"{DAEMON_URL}{path}"
        headers = None
        if _http_mcp_auth_token:
            headers = {"Authorization": f"Bearer {_http_mcp_auth_token}"}
        if method == "GET":
            resp = await client.get(url, params=params, headers=headers)
        elif method == "DELETE":
            resp = await client.delete(url, params=params, headers=headers)
        else:
            resp = await client.post(url, json=body or {}, headers=headers)
        resp.raise_for_status()
        return resp.json()
    except httpx.ConnectError:
        _http_client = None  # Reset stale client so next call reconnects
        raise DaemonConnectionError()
    except httpx.HTTPStatusError as e:
        raise DaemonHTTPError(e.response.status_code, e.response.text)
    except httpx.TimeoutException:
        raise DaemonTimeoutError()


def _metadata_matches_current_process(pane_meta: dict) -> bool:
    """Verify pane runtime metadata describes the *current* process.

    Pane metadata can outlive the session that wrote it (no daemon
    process owns the file; cleanup is best-effort via clear_pane_runtime_state).
    If a daemon restart races with pane reuse, we could read stale metadata
    pointing at a previous tenant's display_name.

    Reject metadata whose cwd or backend mismatches what we observe now.
    """
    meta_cwd = pane_meta.get("cwd")
    meta_backend = pane_meta.get("backend")
    if not meta_cwd or not meta_backend:
        return False
    try:
        current_cwd = str(Path.cwd())
    except OSError:
        return False
    if str(meta_cwd) != current_cwd:
        return False
    return meta_backend == _detect_backend()


async def _get_my_peer_name() -> str:
    """Get own peer name. Cached after first successful resolution.

    Resolution order:
    1. pane-based daemon lookup (most authoritative — handles suffix collisions)
    2. pane runtime metadata file, validated against current cwd+backend
       (covers daemon-not-yet-ready races; rejects stale metadata from a
       prior tenant of a reused pane)
    3. REPOWIRE_DISPLAY_NAME env var or cwd folder name (collides for
       same-cwd peers — last resort, see #107)

    The cwd-folder fallback is intentionally not cached, so a later call
    that can reach the daemon can still resolve the correct suffixed name.
    """
    global _cached_peer_name
    if _cached_peer_name is not None:
        return _cached_peer_name
    pane_id = get_pane_id()
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
            name = result.get("display_name") or result.get("peer_id")
            if name:
                _cache_identity(result)
                return name
        except Exception:
            pass
        pane_meta = read_pane_runtime_metadata(pane_id)
        if _metadata_matches_current_process(pane_meta):
            name = pane_meta.get("display_name") or pane_meta.get("peer_id")
            if name:
                _cached_peer_name = name
                return name
    return get_display_name()


def _detect_backend() -> str:
    """Detect which agent runtime is hosting this MCP server.

    Antigravity CLI (`agy`) has no documented env-var signature today, so
    detection relies on the explicit REPOWIRE_BACKEND override propagated
    via the spawn command profile or hook --backend flag.
    """
    if os.environ.get("GEMINI_CLI"):
        return "gemini"
    if ".codex/" in os.environ.get("PATH", ""):
        return "codex"
    return os.environ.get("REPOWIRE_BACKEND", "claude-code")


def _hook_disconnect_message(pane_id: str) -> str:
    pane_file = pane_id.replace("%", "") or "unknown"
    return (
        "Repowire inbound transport is disconnected for this tmux pane. "
        "The background websocket hook is retrying, but this session is "
        "currently absent from the daemon registry, so outbound messaging is "
        "blocked to avoid masking the failure. Check "
        f"~/.cache/repowire/logs/ws-hook-{pane_file}.log or restart the session "
        "if it does not recover."
    )


async def _touch_last_seen() -> None:
    """Refresh this peer's last_seen on the daemon (best-effort).

    Called from `_ensure_registered` so every MCP tool entry feeds the
    liveness clock. Closes the "ws-hook dropped but agent is alive and
    acting via MCP" gap that would otherwise leave `is_orchestrator_present`
    (and other last_seen-keyed checks) stale.

    On 404/409 (daemon doesn't recognise our cached peer_id — typically
    because the daemon was restarted and our cache is stale), invalidate
    registration so the next MCP call re-resolves identity. Without this
    the cached _cached_peer_id is sent as from_peer on ask/notify and
    ack replies route to a nonexistent peer_id.
    """
    global _registered, _cached_peer_id
    identifier = _cached_peer_id or _cached_peer_name
    if identifier is None:
        return
    try:
        await daemon_request(
            "POST", f"/peers/{quote(identifier, safe='')}/touch",
        )
    except DaemonHTTPError as e:
        if e.status in (404, 409):
            # Cache is stale (daemon restart, peer reassignment, or ambiguous
            # match). Force re-registration on the next call.
            _registered = False
            _cached_peer_id = None
    except Exception:
        pass


async def _ensure_registered(*, strict: bool = False) -> None:
    """Lazy-register this peer with the daemon on first MCP tool use, and
    refresh last_seen on every call.

    Registration short-circuits after the first successful resolve; the
    last_seen touch runs on every entry so MCP activity counts as a liveness
    signal even when ws-hook has dropped.
    """
    global _registered, _cached_peer_name, _cached_my_circle, _cached_my_role, _cached_peer_id
    if _registered:
        await _touch_last_seen()
        # _touch_last_seen invalidates _registered on 404/409 (daemon restart,
        # peer reassignment). Fall through to re-resolve identity in THIS call
        # so the current MCP tool doesn't continue with stale from_peer.
        if _registered:
            return
    if _cached_peer_name is None:
        _cached_peer_id = None

    if _http_mcp_mode:
        try:
            name = _http_mcp_identity.get("name") or "mcp-http"
            circle = _http_mcp_identity.get("circle") or "global"
            role = _http_mcp_identity.get("role") or "human"
            result = await daemon_request(
                "GET", f"/peers/{quote(_cached_peer_id or name, safe='')}",
            )
            _cache_identity(result)
            _registered = True
            await _touch_last_seen()
            return
        except Exception:
            pass
        try:
            result = await daemon_request(
                "POST",
                "/peers",
                {
                    "name": name,
                    "path": "",
                    "machine": "localhost",
                    "backend": "mcp-http",
                    "circle": circle,
                    "role": role,
                    "metadata": {"surface": "http-mcp"},
                },
            )
            _cache_identity(result)
            _cached_my_circle = circle
            _cached_my_role = role
            _registered = True
            await _touch_last_seen()
        except Exception as e:
            if strict:
                raise RuntimeError(f"HTTP MCP identity registration failed: {e}") from e
        return

    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
            name = result.get("display_name") or result.get("peer_id")
            if name:
                _cache_identity(result)
            _registered = True
            await _touch_last_seen()
            return
        except Exception:
            pass

        pane_meta = read_pane_runtime_metadata(pane_id)
        if pane_meta.get("display_name") and _cached_peer_name is None:
            _cached_peer_name = pane_meta["display_name"]
        if strict and (pane_meta.get("peer_id") or pane_meta.get("display_name")):
            raise RuntimeError(_hook_disconnect_message(pane_id))
    else:
        name = await _get_my_peer_name()
        try:
            result = await daemon_request("GET", f"/peers/{quote(name, safe='')}")
            _cache_identity(result)
            _registered = True
            await _touch_last_seen()
            return
        except Exception:
            pass

    backend = _detect_backend()

    try:
        cwd = Path.cwd()
    except OSError as e:
        logger.warning("Cannot resolve cwd for MCP registration: %s", e)
        return

    # Last-resort identity resolution: find hook-registered peer by path+backend.
    # Only claim when there is exactly one online candidate — multiple matches
    # are ambiguous and inheriting an arbitrary peer's identity causes
    # cross-session impersonation (see repowire-c6z).
    try:
        result = await daemon_request("GET", "/peers", params={
            "path": str(cwd),
            "backend": backend,
            "status": "online",
        })
        candidates = result.get("peers", [])
        if len(candidates) == 1:
            assigned = candidates[0].get("display_name")
            if assigned:
                _cache_identity(candidates[0])
                _registered = True
                await _touch_last_seen()
                return
    except (DaemonConnectionError, DaemonHTTPError, DaemonTimeoutError) as e:
        logger.debug("Path+backend peer lookup failed: %s", e)

    # Tmux env is stripped by codex's MCP sandbox, so session_name is None
    # there. Fall back to the spawn hint dropped by the daemon's /spawn route
    # before defaulting to "default". Carry the source so the daemon can
    # distinguish an explicit tmux session named "default" from a true fallback.
    hint = consume_hint_full(str(cwd), backend)
    if tmux_info["session_name"]:
        circle = tmux_info["session_name"]
        circle_source = "tmux"
    elif hint:
        circle = hint["circle"]
        circle_source = "spawn_hint"
    else:
        circle = "default"
        circle_source = "fallback"

    try:
        body: dict = {
            "name": cwd.name or "root",
            "path": str(cwd),
            "circle": circle,
            "backend": backend,
            "circle_source": circle_source,
        }
        if pane_id:
            body["pane_id"] = pane_id
        result = await daemon_request("POST", "/peers", body)
        # Cache the daemon-assigned name
        _cache_identity(result)
        _cached_my_circle = circle
        _cached_my_role = "agent"
        _registered = True
        await _touch_last_seen()
    except Exception:
        pass  # Best-effort -- daemon may be down


def _ensure_http_admin_tool_allowed(tool_name: str) -> None:
    """Keep lifecycle/admin tools off the first HTTP MCP slice by default."""
    if _http_mcp_mode and not _http_mcp_allow_dangerous_tools:
        raise PermissionError(
            f"{tool_name} is disabled for HTTP MCP by default; "
            "enable daemon.mcp_http.allow_dangerous_tools to opt in"
        )


def create_mcp_server(*, streamable_http_path: str = "/mcp") -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire", streamable_http_path=streamable_http_path)

    # New columns append at the end so positional-indexing TSV consumers
    # survive across releases (same rule that put last_seen at the tail in
    # #138). Keep turn_state last for the same reason.
    tsv_header = (
        "peer_id\tname\tproject\tcircle\trole\tstatus\tpath\t"
        "machine\tdescription\tbackend\tlast_seen\tturn_state"
    )

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
                p.get("last_seen") or "",
                p.get("turn_state") or "",
            ]
        )

    @mcp.tool()
    async def list_peers(
        show_offline: bool = False,
        include_self: bool = False,
        circle: str | None = None,
    ) -> str:
        """[Repowire mesh] List peers, scoped to your circle by default.

        By default, agent callers see only online/busy peers in their own
        circle and hide the calling peer. Peers whose role bypasses circles
        (service, orchestrator, human) are visible with concrete circle
        filters. Pass `circle='*'` for a mesh-wide listing, or a concrete
        circle name to inspect that circle plus bypass peers.

        Callers with role=orchestrator default to mesh-wide (`*`) when
        `circle` is omitted.

        Set show_offline=True to include offline peers, or include_self=True
        to include yourself in the listing.

        Returns TSV: peer_id, name, project, circle, role, status, path,
        machine, description, backend, last_seen, turn_state. The turn_state
        column is "" when unknown; otherwise idle, working, awaiting_input
        (peer is waiting on user input mid-turn), or pending_first_turn (peer
        was spawn-seeded but the seed never reached the agent -- send a
        follow-up via ask or notify_peer). Use ask/notify_peer for repowire
        peers; SendMessage is only for same-session Claude Code harness
        teammates, not mesh peers.
        """
        await _ensure_registered()
        my_name, my_circle, my_role = await _get_my_identity()

        effective_circle: str | None
        if circle is not None:
            effective_circle = circle
        elif my_role == "orchestrator":
            effective_circle = "*"
        else:
            effective_circle = my_circle

        params: dict[str, str] = {}
        if not show_offline:
            params["status"] = "online"
        if effective_circle is not None and effective_circle != "*":
            params["circle"] = effective_circle
        result = await daemon_request("GET", "/peers", params=params or None)
        peers = result.get("peers", [])
        if not include_self and my_name:
            peers = [
                p for p in peers
                if (p.get("display_name") or p.get("name")) != my_name
            ]
        rows = [tsv_header]
        for p in peers:
            rows.append(_peer_to_tsv_row(p))
        return "\n".join(rows)

    @mcp.tool()
    async def ask(
        peer_name: str,
        query: str,
        reply_to: str | None = None,
        circle: str | None = None,
        attachments: list[dict] | None = None,
    ) -> str:
        """[Repowire mesh] Open a non-blocking ask thread with a peer.

        Use for tracked work that the recipient must close with `ack`. The
        call returns a correlation_id immediately; it is not a synchronous
        wait or delivery receipt. Use notify_peer for fire-and-forget nudges.
        Chain follow-ups with `reply_to`, which closes the prior thread and
        opens a new one. Do not use SendMessage for mesh peers.

        Args:
            peer_name: Display name or peer_id of the peer to ask.
            query: The question or request to send
            reply_to: If set, closes that prior ask before opening this one
            circle: Optional target lookup circle.
            attachments: Optional daemon attachment metadata objects.

        Returns:
            correlation_id for tracking this ask thread
        """
        await _ensure_registered(strict=True)
        from_peer_name, my_circle, _ = await _get_my_identity()
        from_peer = _cached_peer_id or from_peer_name
        effective_circle = await _resolve_circle_for_send(peer_name, circle, my_circle)
        body: dict = {
            "from_peer": from_peer,
            "to_peer": peer_name,
            "text": query,
        }
        if reply_to is not None:
            body["reply_to"] = reply_to
        if effective_circle is not None:
            body["circle"] = effective_circle
        if attachments:
            body["attachments"] = attachments
        result = await daemon_request("POST", "/ask", body)
        if result.get("error"):
            raise Exception(result["error"])
        return result.get("correlation_id", "")

    @mcp.tool()
    async def ack(
        correlation_id: str,
        message: str | None = None,
        attachments: list[dict] | None = None,
    ) -> str:
        """[Repowire mesh] Close an open ask thread.

        Bare ack: `ack(corr_id)` — closes the thread, signals "seen, no
        action needed." Use when an ask doesn't require a substantive reply.

        Reply ack: `ack(corr_id, message)` — IS the reply. Closes the thread
        AND delivers the message back to the original asker, framed as
        `[ack #corr_id from @you] message` via the notification pipeline.

        Replies always reach the original asker, regardless of circle (the
        thread was already established at ask-time).

        Args:
            correlation_id: The ask's correlation_id
            message: Optional reply content. Omit for bare close.
            attachments: Optional daemon attachment metadata objects to send
                    with a reply.

        Returns:
            Confirmation message
        """
        await _ensure_registered(strict=True)
        from_peer = await _get_my_peer_identifier()
        body: dict = {
            "correlation_id": correlation_id,
            "from_peer": from_peer,
        }
        if message is not None:
            body["message"] = message
        if attachments:
            body["attachments"] = attachments
        await daemon_request("POST", "/ack", body)
        return f"acked #{correlation_id}" + (
            " with reply" if (message or attachments) else ""
        )

    @mcp.tool()
    async def notify_peer(
        peer_name: str,
        message: str,
        circle: str | None = None,
        attachments: list[dict] | None = None,
    ) -> str:
        """[Repowire mesh] Send a fire-and-forget notification to a peer in another project.

        Use for status updates, announcements, replies to notifications,
        reminders, self-wakes, and nudges that do not require closure.
        Special peers: 'telegram' sends to user's phone.
        The dashboard sees your responses automatically via chat turns - no
        need to notify it.
        Fire-and-forget means daemon-side success does NOT guarantee agent receipt.
        Use ask() when the work needs a tracked thread and explicit ack.

        Do not use SendMessage to reach repowire peers. SendMessage is a Claude
        Code harness tool for same-session teammates only.

        Args:
            peer_name: Display name or peer_id of the peer to notify.
            message: The notification message
            circle: Circle to scope the lookup. Defaults to your own circle,
                    with fallback to peers whose role bypasses circles
                    (service, orchestrator, human). Pass explicitly to target
                    a peer in a different circle.
            attachments: Optional daemon attachment metadata objects. Existing
                    text-only callers can omit this.

        Returns:
            Correlation ID (format: notif-XXXXXXXX) for tracking.
        """
        await _ensure_registered(strict=True)
        from_peer_name, my_circle, _ = await _get_my_identity()
        from_peer = _cached_peer_id or from_peer_name
        effective_circle = await _resolve_circle_for_send(peer_name, circle, my_circle)
        correlation_id = f"notif-{uuid4().hex[:8]}"
        body: dict = {
            "from_peer": from_peer,
            "to_peer": peer_name,
            "text": f"[#{correlation_id}] {message}",
        }
        if effective_circle is not None:
            body["circle"] = effective_circle
        if attachments:
            body["attachments"] = attachments
        await daemon_request("POST", "/notify", body)
        return correlation_id

    @mcp.tool()
    async def broadcast(message: str) -> str:
        """[Repowire mesh] Broadcast to all online peers across the mesh.

        Use for announcements that affect everyone, like deployment updates
        or breaking changes. Do not use for replies or tracked work; use
        ack/ask for ask threads and notify_peer for one peer.

        Do not use SendMessage to reach repowire peers. SendMessage is a Claude
        Code harness tool for same-session teammates only.

        Args:
            message: The message to broadcast

        Returns:
            Confirmation message
        """
        await _ensure_registered(strict=True)
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
        failed = result.get("failed", [])
        parts = []
        if sent_to:
            parts.append(f"Broadcast sent to: {', '.join(sent_to)}")
        else:
            parts.append("Broadcast sent to: no peers online")
        if failed:
            failures = ", ".join(
                f"{f.get('peer', '?')} ({f.get('error', 'unknown')})"
                for f in failed
            )
            parts.append(f"Failed: {failures}")
        return "; ".join(parts)

    def _format_peer_tsv(result: dict) -> str:
        """Format a peer result dict as a TSV row with header."""
        return f"{tsv_header}\n{_peer_to_tsv_row(result)}"

    @mcp.tool()
    async def whoami() -> str:
        """[Repowire mesh] Return your identity in the repowire mesh.

        Returns TSV with columns: peer_id, name, project, circle, role, status,
        path, machine, description, backend, last_seen, turn_state
        """
        await _ensure_registered(strict=True)
        pane_id = None if _http_mcp_mode else get_pane_id()
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
                return _format_peer_tsv(result)
            except Exception:
                pass  # fall through to fallback

        name = await _get_my_peer_name()
        try:
            identifier = _cached_peer_id or name
            result = await daemon_request("GET", f"/peers/{quote(identifier, safe='')}")
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
        await _ensure_registered(strict=True)
        pane_id = None if _http_mcp_mode else get_pane_id()
        name = ""
        if pane_id:
            try:
                result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
                name = result.get("display_name") or result.get("name", "")
            except Exception as e:
                logger.warning("Could not get peer name by pane_id '%s': %s", pane_id, e)
        if not name:
            name = await _get_my_peer_name()
        identifier = _cached_peer_id or name
        await daemon_request(
            "POST",
            f"/peers/{quote(identifier, safe='')}/description",
            {"description": description},
        )
        return f"description updated: {description}"

    @mcp.tool()
    async def claim_orchestrator_role(force: bool = False) -> str:
        """[Repowire mesh] Reclaim this session's role=orchestrator.

        Use from the orchestrator workspace after a daemon restart if list_peers
        shows this session as role=agent. This is self-only: it targets the
        caller's current peer_id and cannot claim the role for another peer.

        Args:
            force: Demote an existing fresh orchestrator holder in this circle.

        Returns:
            Confirmation message
        """
        _ensure_http_admin_tool_allowed("claim_orchestrator_role")
        await _ensure_registered(strict=True)
        name, circle, _role = await _get_my_identity()
        identifier = _cached_peer_id or name
        if not identifier:
            raise RuntimeError("Cannot resolve current peer identity")

        current = await daemon_request("GET", f"/peers/{quote(identifier, safe='')}")
        _cache_identity(current)
        if not _orchestrator_self_claim_allowed(current):
            raise PermissionError(
                "claim_orchestrator_role can only be used by the orchestrator "
                "workspace/session"
            )

        target = _cached_peer_id or identifier
        result = await daemon_request(
            "POST",
            "/peers/claim-role",
            {
                "role": "orchestrator",
                "peer_name": target,
                "circle": current.get("circle") or circle,
                "force": force,
            },
        )
        _cache_identity(result)
        holders = result.get("previous_holders") or []
        suffix = f"; demoted {len(holders)} previous holder(s)" if holders else ""
        return (
            f"claimed role=orchestrator for {result.get('peer_name') or target} "
            f"in circle {result.get('circle')}{suffix}"
        )

    @mcp.tool()
    async def spawn_peer(
        path: str,
        backend: str | None = None,
        profile: str | None = None,
        command: str | None = None,
        circle: str | None = None,
        message: str | None = None,
    ) -> str:
        """[Repowire mesh] Spawn a new coding session in a different project directory.

        Prefer `backend` (claude-code, codex, gemini, antigravity, opencode, pi).
        The backend
        must have a command configured in daemon.spawn.commands in
        ~/.repowire/config.yaml. Pass `profile` to append configured
        daemon.spawn.profiles args for that backend. `command` is retained as a
        one-release compatibility alias for older callers.

        The spawned agent self-registers into the mesh via its SessionStart hook
        within a few seconds. Use list_peers() to confirm registration and get
        the peer_id.

        The circle maps to the tmux session name and cannot be reassigned after
        spawn. If omitted, Repowire uses the caller's current circle.

        Pass `message` to seed the spawned agent with first-turn context (task
        brief, who spawned them, what to work on). Required for codex peers to
        register with the mesh promptly; treated as a friendly opening prompt
        by other backends. If omitted, codex gets a short default warmup.

        After spawn, address the peer by the returned display name or by the
        peer_id from list_peers. Use ask() for tracked work that requires ack,
        or notify_peer() for fire-and-forget prompts. Do not use SendMessage;
        it is a Claude Code harness tool for same-session teammates only.

        Args:
            path: Absolute path to the project directory
            backend: Runtime profile to spawn (e.g. "claude-code", "codex")
            profile: Optional named spawn model/profile for the backend
            command: Deprecated compatibility command/profile selector
            circle: Circle to spawn into (default: caller's current circle)
                    -- maps to tmux session name
            message: Optional first-turn prompt for the spawned agent. Codex
                     needs it (or the default warmup) to register promptly.

        Returns:
            Spawn confirmation with display_name and tmux_session
        """
        _ensure_http_admin_tool_allowed("spawn_peer")
        if backend is not None and command is not None:
            raise ValueError("Pass backend or command, not both")
        if backend is None and command is None:
            raise ValueError("Pass backend or command")
        if profile is not None and backend is None:
            raise ValueError("Pass backend with profile")
        if circle is None:
            await _ensure_registered(strict=True)
            _name, my_circle, _role = await _get_my_identity()
            circle = my_circle or "default"
        body: dict = {"path": path, "circle": circle}
        if backend is not None:
            body["backend"] = backend
        if profile is not None:
            body["profile"] = profile
        if command is not None:
            body["command"] = command
        if message is not None:
            body["message"] = message
        result = await daemon_request("POST", "/spawn", body)
        name = result["display_name"]
        tmux = result["tmux_session"]
        return (
            f"Spawned {name} (tmux: {tmux}). "
            f"Peer will self-register shortly. Use list_peers() to confirm "
            f"and get peer_id. Address it as '{name}' via ask/notify_peer."
        )

    @mcp.tool()
    async def orchestrator_status(circle: str | None = None) -> str:
        """[Repowire mesh] Check whether a live orchestrator is present in a circle.

        Useful before dispatching long-running work that assumes an orchestrator
        will be available to coordinate. "Live" = role=orchestrator, status
        online/busy, and last heartbeat within 2 * heartbeat_interval (one
        missed beat tolerated).

        Args:
            circle: Circle to check. Defaults to your own circle.

        Returns:
            TSV row: circle, present, peer_name, peer_id, last_seen, stale_after_seconds
        """
        await _ensure_registered()
        target_circle = circle
        if target_circle is None:
            try:
                me = await daemon_request(
                    "GET", f"/peers/{await _get_my_peer_name()}",
                )
                target_circle = me.get("circle") or "global"
            except Exception:
                target_circle = "global"
        result = await daemon_request(
            "GET", f"/circles/{quote(target_circle, safe='')}/orchestrator",
        )
        header = "circle\tpresent\tpeer_name\tpeer_id\tlast_seen\tstale_after_seconds"
        row = "\t".join(
            [
                result.get("circle", target_circle),
                "true" if result.get("present") else "false",
                result.get("peer_name") or "",
                result.get("peer_id") or "",
                result.get("last_seen") or "",
                str(result.get("stale_after_seconds", "")),
            ]
        )
        return f"{header}\n{row}"

    @mcp.tool()
    async def kill_peer(peer_identifier: str, circle: str | None = None) -> str:
        """[Repowire mesh] Kill a registered local coding session.

        The peer is always deregistered from the mesh. The tmux pane behind
        it is only killed if the daemon can prove Repowire spawned it, either
        from current in-memory ownership or durable spawn ownership plus live
        tmux evidence. Externally attached peers and stale/mismatched pane
        records are deregistered without touching tmux — verify and follow up
        with `tmux kill-pane` if the pane survives.

        Args:
            peer_identifier: peer_id or display name from list_peers.
            circle: Optional circle to disambiguate display names.

        Returns:
            Confirmation describing both the deregistration and the tmux
            pane outcome.
        """
        await _ensure_registered(strict=True)
        _ensure_http_admin_tool_allowed("kill_peer")
        payload: dict[str, str] = {
            "peer_identifier": peer_identifier,
            "from_peer": await _get_my_peer_name(),
        }
        if circle is not None:
            payload["circle"] = circle
        result = await daemon_request("POST", "/kill-peer", payload)
        scoped = f" in circle {circle}" if circle else ""
        tmux_killed = (result or {}).get("tmux_killed")
        if tmux_killed is True:
            tmux_note = "tmux pane killed"
        elif tmux_killed is False:
            tmux_note = "tmux pane kill attempted but failed (verify with `tmux list-panes`)"
        else:
            tmux_note = (
                "tmux pane kill skipped (daemon ownership not proven — "
                "externally attached, stale, or mismatched pane evidence). "
                "Verify with `tmux list-panes` and manually `tmux kill-pane` if needed."
            )
        return f"Killed peer {peer_identifier}{scoped}: {tmux_note}"

    @mcp.tool()
    async def mark_reviewed(pr_url: str, last_reviewed_sha: str | None = None) -> str:
        """[Repowire mesh] Record that you've reviewed a PR.

        Use this after reviewing a GitHub pull request so it stops surfacing
        in your `review_queue`. If `last_reviewed_sha` is omitted the daemon
        best-effort fetches the current HEAD via `gh api`; future pushes to
        the PR will then show up as `re-review-suggested`.

        Args:
            pr_url: Full GitHub PR URL (https://github.com/owner/repo/pull/N)
            last_reviewed_sha: SHA you just reviewed. Optional.

        Returns:
            Confirmation message.
        """
        await _ensure_registered(strict=True)
        reviewer = await _get_my_peer_name()
        body: dict = {"reviewer": reviewer, "pr_url": pr_url}
        if last_reviewed_sha is not None:
            body["last_reviewed_sha"] = last_reviewed_sha
        await daemon_request("POST", "/reviews", body)
        return f"marked reviewed: {pr_url}"

    @mcp.tool()
    async def review_queue(peer_name: str | None = None) -> str:
        """[Repowire mesh] List PRs awaiting your review.

        Defaults to the calling peer. Returns TSV with columns:
        pr_url, last_reviewed_sha, current_head_sha, state, my_action.

        my_action values:
          - none-needed:           PR open, head SHA matches what you reviewed
          - re-review-suggested:   PR open, new commits since your review
          - merged-since-review:   PR merged after you last reviewed it
          - closed-since-review:   PR closed (not merged) since your review
          - unknown:               GitHub API unreachable; falls back to cached state

        Args:
            peer_name: Reviewer to query. Defaults to you.

        Returns:
            TSV rows, header included.
        """
        await _ensure_registered(strict=True)
        reviewer = peer_name or await _get_my_peer_name()
        result = await daemon_request("GET", "/reviews", params={"reviewer": reviewer})
        header = "pr_url\tlast_reviewed_sha\tcurrent_head_sha\tstate\tmy_action"
        rows = [header]
        for item in result.get("reviews", []):
            rows.append(
                "\t".join(
                    [
                        item.get("pr_url", ""),
                        item.get("last_reviewed_sha") or "",
                        item.get("current_head_sha") or "",
                        item.get("state", ""),
                        item.get("my_action", ""),
                    ]
                )
            )
        return "\n".join(rows)

    @mcp.tool()
    async def schedule_create(
        to_peer: str,
        text: str,
        fire_at: str,
        kind: str = "notify",
        circle: str | None = None,
    ) -> str:
        """[Repowire mesh] Schedule a one-shot future message to a peer.

        At `fire_at` the daemon will deliver `text` to `to_peer` on your
        behalf. Use `kind="notify"` (default) for fire-and-forget reminders,
        self-wakes, nudges, and check-ins that do not need closure. Use
        `kind="ask"` only when the future delivery should open a tracked ask
        thread that requires `ack`/closure, such as a worker status check or
        reviewer checkpoint.

        Use schedule_cron for recurring peer messages, or schedule_self when
        the recipient is your own peer.

        Args:
            to_peer: Display name or peer_id of the recipient. Use your own
                     peer name to schedule a self-wake.
            text: Message body to deliver.
            fire_at: ISO-8601 datetime (e.g. "2026-05-14T09:00:00Z" or
                     "2026-05-14T09:00:00+00:00"). Naive datetimes are
                     interpreted as UTC.
            kind: "notify" (default) for reminders/self-wakes/nudges, or
                  "ask" when delivery should open a tracked ask thread.
            circle: Circle to scope recipient lookup.

        Returns:
            schedule_id (format: sched-XXXXXXXX). Use it with
            schedule_delete to cancel.
        """
        await _ensure_registered(strict=True)
        _ensure_http_admin_tool_allowed("schedule_create")
        from_peer = await _get_my_peer_name()
        body: dict = {
            "from_peer": from_peer,
            "to_peer": to_peer,
            "text": text,
            "fire_at": fire_at,
            "kind": kind,
        }
        if circle is not None:
            body["circle"] = circle
        result = await daemon_request("POST", "/schedules", body)
        return result.get("schedule_id", "")

    @mcp.tool()
    async def schedule_self(
        text: str,
        fire_at: str | None = None,
        cron: str | None = None,
        kind: str = "notify",
        circle: str | None = None,
    ) -> str:
        """[Repowire mesh] Schedule a future message to yourself.

        Convenience wrapper around schedule_create for self-wake reminders.
        For one-time schedules, pass `fire_at`. For recurring schedules, pass
        a five-field `cron` expression (or aliases like `@hourly`, `@daily`,
        `@midnight`, `@weekly`, or `@monthly`).
        Use `kind="notify"` (default) for fire-and-forget reminders,
        self-wakes, and nudges. Use `kind="ask"` only when the future message
        should create a tracked ask thread that requires `ack`/closure.

        Args:
            text: Message body to deliver to yourself.
            fire_at: ISO-8601 datetime (e.g. "2026-05-14T09:00:00Z" or
                     "2026-05-14T09:00:00+00:00"). Naive datetimes are
                     interpreted as UTC.
            cron: Five-field cron expression or supported alias.
            kind: "notify" (default) for reminders/self-wakes/nudges, or
                  "ask" when delivery should open a tracked ask thread.
            circle: Circle to scope recipient lookup.

        Returns:
            schedule_id (format: sched-XXXXXXXX). Use it with
            schedule_delete to cancel.
        """
        await _ensure_registered(strict=True)
        _ensure_http_admin_tool_allowed("schedule_self")
        if (fire_at is None) == (cron is None):
            raise ValueError("provide exactly one of fire_at or cron")
        from_peer = await _get_my_peer_name()
        body: dict = {
            "from_peer": from_peer,
            "to_peer": from_peer,
            "text": text,
            "kind": kind,
        }
        if fire_at is not None:
            body["fire_at"] = fire_at
        if cron is not None:
            body["cron"] = cron
        if circle is not None:
            body["circle"] = circle
        result = await daemon_request("POST", "/schedules", body)
        return result.get("schedule_id", "")

    @mcp.tool()
    async def schedule_cron(
        to_peer: str,
        text: str,
        cron: str,
        kind: str = "notify",
        circle: str | None = None,
    ) -> str:
        """[Repowire mesh] Schedule a recurring cron message to a peer.

        Use `kind="notify"` (default) for recurring reminders, self-wakes,
        nudges, and fire-and-forget check-ins. Use `kind="ask"` only when each
        recurring delivery should open a tracked ask thread that requires
        `ack`/closure, such as a worker status check or reviewer checkpoint.

        Args:
            to_peer: Display name or peer_id of the recipient.
            text: Message body to deliver.
            cron: Five-field cron expression, or alias such as `@hourly`,
                  `@daily`, `@midnight`, `@weekly`, or `@monthly`.
            kind: "notify" (default) for reminders/self-wakes/nudges, or
                  "ask" when delivery should open a tracked ask thread.
            circle: Circle to scope recipient lookup.

        Returns:
            schedule_id (format: sched-XXXXXXXX). Use schedule_delete to cancel.
        """
        await _ensure_registered(strict=True)
        _ensure_http_admin_tool_allowed("schedule_cron")
        from_peer = await _get_my_peer_name()
        body: dict = {
            "from_peer": from_peer,
            "to_peer": to_peer,
            "text": text,
            "cron": cron,
            "kind": kind,
        }
        if circle is not None:
            body["circle"] = circle
        result = await daemon_request("POST", "/schedules", body)
        return result.get("schedule_id", "")

    @mcp.tool()
    async def schedule_list(mine_only: bool = True, include_cron: bool = False) -> str:
        """[Repowire mesh] List pending scheduled check-ins.

        Returns TSV with columns: schedule_id, from_peer, to_peer, kind,
        fire_at, text. Includes one-shot and recurring schedules, sorted by
        next fire_at ascending. Pass include_cron=True to append a trailing
        cron column for recurring schedules.

        Args:
            mine_only: If True (default), return only schedules you
                       created. If False, return all schedules on the
                       daemon.
            include_cron: If True, append a trailing cron column. Defaults
                          False to preserve the original TSV schema.
        """
        await _ensure_registered()
        params: dict | None = None
        if mine_only:
            params = {"from_peer": await _get_my_peer_name()}
        result = await daemon_request("GET", "/schedules", params=params)
        header = "schedule_id\tfrom_peer\tto_peer\tkind\tfire_at\ttext"
        if include_cron:
            header += "\tcron"
        rows = [header]
        for s in result.get("schedules", []):
            fields = [
                s.get("schedule_id", ""),
                s.get("from_peer", ""),
                s.get("to_peer", ""),
                s.get("kind", ""),
                s.get("fire_at", ""),
                (s.get("text", "") or "").replace("\t", " ").replace("\n", " "),
            ]
            if include_cron:
                fields.append(s.get("cron") or "")
            rows.append("\t".join(fields))
        return "\n".join(rows)

    @mcp.tool()
    async def schedule_delete(schedule_id: str) -> str:
        """[Repowire mesh] Cancel a pending scheduled check-in.

        Args:
            schedule_id: ID returned by schedule_create, schedule_self, or
                         schedule_cron.

        Returns:
            Confirmation message.
        """
        await _ensure_registered(strict=True)
        _ensure_http_admin_tool_allowed("schedule_delete")
        await daemon_request("DELETE", f"/schedules/{quote(schedule_id, safe='')}")
        return f"deleted schedule {schedule_id}"

    return mcp


async def run_mcp_server() -> None:
    """Run the MCP server."""
    reset_mcp_context()
    mcp = create_mcp_server()
    await mcp.run_stdio_async()
