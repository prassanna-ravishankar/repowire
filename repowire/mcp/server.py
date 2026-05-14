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
from repowire.spawn_hints import consume_hint

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


async def daemon_request(
    method: str, path: str, body: dict | None = None, params: dict | None = None
) -> dict:
    """Make an HTTP request to the daemon."""
    global _http_client
    try:
        client = _get_http_client()
        url = f"{DAEMON_URL}{path}"
        if method == "GET":
            resp = await client.get(url, params=params)
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
                _cached_peer_name = name
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
    """Detect which agent runtime is hosting this MCP server."""
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
    """
    if _cached_peer_name is None:
        return
    try:
        await daemon_request(
            "POST", f"/peers/{quote(_cached_peer_name, safe='')}/touch",
        )
    except Exception:
        pass


async def _ensure_registered(*, strict: bool = False) -> None:
    """Lazy-register this peer with the daemon on first MCP tool use, and
    refresh last_seen on every call.

    Registration short-circuits after the first successful resolve; the
    last_seen touch runs on every entry so MCP activity counts as a liveness
    signal even when ws-hook has dropped.
    """
    global _registered, _cached_peer_name
    if _registered:
        await _touch_last_seen()
        return

    tmux_info = get_tmux_info()
    pane_id = tmux_info["pane_id"]
    if pane_id:
        try:
            result = await daemon_request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
            name = result.get("display_name") or result.get("peer_id")
            if name:
                _cached_peer_name = name
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
            await daemon_request("GET", f"/peers/{quote(name, safe='')}")
            _cached_peer_name = name
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
                _cached_peer_name = assigned
                _registered = True
                await _touch_last_seen()
                return
    except (DaemonConnectionError, DaemonHTTPError, DaemonTimeoutError) as e:
        logger.debug("Path+backend peer lookup failed: %s", e)

    # Tmux env is stripped by codex's MCP sandbox, so session_name is None
    # there. Fall back to the spawn hint dropped by the daemon's /spawn route
    # before defaulting to "default".
    circle = tmux_info["session_name"] or consume_hint(str(cwd), backend) or "default"

    try:
        body: dict = {
            "name": cwd.name or "root",
            "path": str(cwd),
            "circle": circle,
            "backend": backend,
        }
        if pane_id:
            body["pane_id"] = pane_id
        result = await daemon_request("POST", "/peers", body)
        # Cache the daemon-assigned name
        assigned = result.get("display_name")
        if assigned:
            _cached_peer_name = assigned
        _registered = True
        await _touch_last_seen()
    except Exception:
        pass  # Best-effort -- daemon may be down


def create_mcp_server() -> FastMCP:
    """Create the MCP server."""
    mcp = FastMCP("repowire")

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
    async def list_peers(show_offline: bool = False, include_self: bool = False) -> str:
        """[Repowire mesh] List all peers across projects and machines.

        By default shows only online/busy peers and hides the calling peer
        (you). Set show_offline=True to include offline peers, or
        include_self=True to include yourself in the listing.

        Returns TSV: peer_id, name, project, circle, role, status, path,
        machine, description, backend, last_seen, turn_state. The turn_state
        column is "" when unknown; otherwise idle, working, awaiting_input
        (peer is waiting on user input mid-turn), or pending_first_turn (peer
        was spawn-seeded but the seed never reached the agent -- re-send via
        notify_peer). Peers are reachable via ask/notify_peer. Do NOT
        use SendMessage to contact them -- SendMessage is a Claude Code harness
        tool for same-session teammates only.
        """
        await _ensure_registered()
        params = None if show_offline else {"status": "online"}
        result = await daemon_request("GET", "/peers", params=params)
        peers = result.get("peers", [])
        if not include_self:
            my_name = _cached_peer_name
            if my_name:
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
    ) -> str:
        """[Repowire mesh] Open a non-blocking ask thread with a peer.

        Returns immediately with a correlation_id. The peer receives the
        ask, and when they respond they call `ack(correlation_id)` (bare
        close, "seen, no action") or `ack(correlation_id, message)` (close
        with reply content delivered back to you as a notification framed
        `[ack #correlation_id from @peer] message`).

        To chain a follow-up: call `ask(peer_name, query, reply_to=corr_id)`.
        That closes the prior thread AND opens a new one referencing it.

        If you need a synchronous wait, write your own poll loop on the
        notification stream — the MCP surface is non-blocking by design.

        Do NOT use SendMessage to reach repowire peers. SendMessage is a
        Claude Code harness tool for same-session teammates only.

        Args:
            peer_name: Name of the peer to ask
            query: The question or request to send
            reply_to: If set, closes that prior ask before opening this one
            circle: Circle to scope the lookup (optional)

        Returns:
            correlation_id for tracking this ask thread
        """
        await _ensure_registered(strict=True)
        from_peer = await _get_my_peer_name()
        body: dict = {
            "from_peer": from_peer,
            "to_peer": peer_name,
            "text": query,
        }
        if reply_to is not None:
            body["reply_to"] = reply_to
        if circle is not None:
            body["circle"] = circle
        result = await daemon_request("POST", "/ask", body)
        if result.get("error"):
            raise Exception(result["error"])
        return result.get("correlation_id", "")

    @mcp.tool()
    async def ack(correlation_id: str, message: str | None = None) -> str:
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

        Returns:
            Confirmation message
        """
        await _ensure_registered(strict=True)
        from_peer = await _get_my_peer_name()
        body: dict = {
            "correlation_id": correlation_id,
            "from_peer": from_peer,
        }
        if message is not None:
            body["message"] = message
        await daemon_request("POST", "/ack", body)
        return f"acked #{correlation_id}" + (" with reply" if message else "")

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
        await _ensure_registered(strict=True)
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
        await _ensure_registered(strict=True)
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
    async def spawn_peer(
        path: str,
        command: str,
        circle: str = "default",
        message: str | None = None,
    ) -> str:
        """[Repowire mesh] Spawn a new coding session in a different project directory.

        The command must exactly match an entry in daemon.spawn.allowed_commands
        in ~/.repowire/config.yaml. If no allowed_commands are configured, spawn
        is disabled and this will return an error.

        The spawned agent self-registers into the mesh via its SessionStart hook
        within a few seconds. Use list_peers() to confirm registration and get
        the peer_id.

        The circle maps to the tmux session name and cannot be reassigned after
        spawn.

        Pass `message` to seed the spawned agent with first-turn context (task
        brief, who spawned them, what to work on). Required for codex peers to
        register with the mesh promptly; treated as a friendly opening prompt
        by other backends. If omitted, codex gets a short default warmup.

        Do NOT use SendMessage to reach spawned peers. SendMessage is a Claude
        Code harness tool for same-session teammates only. Use ask() or
        notify_peer() instead.

        Args:
            path: Absolute path to the project directory
            command: Command to run (e.g. "claude", "claude --dangerously-skip-permissions")
            circle: Circle to spawn into (default: "default") -- maps to tmux session name
            message: Optional first-turn prompt for the spawned agent. Codex
                     requires it (or a default) to fire its SessionStart hook.

        Returns:
            Spawn confirmation with display_name and tmux_session
        """
        body: dict = {"path": path, "command": command, "circle": circle}
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
        it is only killed if the daemon spawned the peer via spawn_peer in
        the current daemon lifetime. Externally attached peers, or peers
        whose ownership was lost across a daemon restart, are deregistered
        without touching tmux — verify and follow up with `tmux kill-pane`
        if the pane survives.

        Args:
            peer_identifier: Peer ID or display name from list_peers.
            circle: Optional circle to disambiguate display names.

        Returns:
            Confirmation describing both the deregistration and the tmux
            pane outcome.
        """
        await _ensure_registered(strict=True)
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
                "externally attached, or daemon restarted since spawn). "
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

    return mcp


async def run_mcp_server() -> None:
    """Run the MCP server."""
    mcp = create_mcp_server()
    await mcp.run_stdio_async()
