"""Claudemux backend - tmux-based message delivery for Claude Code.

This backend delivers messages via tmux panes and receives responses
via the Stop hook system. The pending file mechanism is used to allow
the stop_handler to correlate responses with queries.

Query flow:
1. send_query() writes correlation info to ~/.repowire/pending/{peer_id}.json
2. Message sent to tmux pane
3. Claude processes and responds
4. Stop hook fires, reads pending file, extracts response from transcript
5. Hook POSTs response to daemon's /hook/response endpoint
6. daemon.core.resolve_hook_response() resolves the query via QueryTracker
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timezone
from pathlib import Path
from typing import TYPE_CHECKING

import libtmux

from repowire.backends.base import Backend
from repowire.backends.claudemux.installer import (
    check_hooks_installed,
    install_hooks,
    uninstall_hooks,
)
from repowire.protocol.peers import PeerStatus

if TYPE_CHECKING:
    from repowire.config.models import PeerConfig
    from repowire.daemon.query_tracker import QueryTracker

logger = logging.getLogger(__name__)


class ClaudemuxBackend(Backend):
    """Backend for Claude Code sessions running in tmux.

    For query tracking, this backend:
    - Writes pending files for the Stop hook to find correlation info
    - Delegates actual Future management to QueryTracker (if provided)
    - Falls back to internal tracking if no QueryTracker (for backward compat)
    """

    name = "claudemux"

    def __init__(self, query_tracker: QueryTracker | None = None) -> None:
        """Initialize the claudemux backend.

        Args:
            query_tracker: Optional centralized query tracker. If provided,
                          query resolution is delegated to it.
        """
        self._server: libtmux.Server | None = None
        self._query_tracker = query_tracker
        self._pending_dir = Path.home() / ".repowire" / "pending"

    async def start(self) -> None:
        """Initialize tmux server connection."""
        self._server = libtmux.Server()
        self._pending_dir.mkdir(parents=True, exist_ok=True)

    async def stop(self) -> None:
        """Cleanup."""
        self._server = None

    async def send_message(self, peer: PeerConfig, text: str) -> None:
        """Send a fire-and-forget message to a peer's tmux pane."""
        pane = self._get_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer.name}")

        pane.send_keys(text, enter=True)
        pane.send_keys("", enter=True)  # Extra Enter for robustness when peer is busy

    async def send_query(
        self,
        peer: PeerConfig,
        text: str,
        timeout: float = 120.0,
        *,
        from_peer: str = "daemon",
        correlation_id: str | None = None,
    ) -> str:
        """Send a query and wait for response via hook callback.

        Args:
            peer: Target peer configuration
            text: Query text to send
            timeout: Timeout in seconds
            from_peer: Name of the sending peer (for QueryTracker)
            correlation_id: Optional correlation ID (from QueryTracker)

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer's tmux pane not found
            TimeoutError: If no response within timeout
        """
        import asyncio
        from uuid import uuid4

        pane = self._get_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer.name}")

        # Use provided correlation_id or generate one
        if correlation_id is None:
            correlation_id = str(uuid4())

        # Get peer_id for pending file naming
        # Use effective_peer_id which handles legacy configs
        peer_id = peer.effective_peer_id

        # Store correlation_id in pending file for stop_handler to find
        # File is named by peer_id so stop_handler can find it by TMUX_PANE
        pending_filename = self._peer_id_to_filename(peer_id)
        pending_file = self._pending_dir / f"{pending_filename}.json"
        pending_data = {
            "correlation_id": correlation_id,
            "to_peer": peer.name,
            "to_peer_id": peer_id,
            "query": text,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        pending_file.write_text(json.dumps(pending_data))

        # Get future from QueryTracker if available
        future: asyncio.Future[str] | None = None
        if self._query_tracker:
            future = self._query_tracker.get_future(correlation_id)

        if future is None:
            # No QueryTracker or correlation_id not registered - shouldn't happen
            # in normal flow but handle gracefully
            logger.warning(
                f"No future found for correlation_id {correlation_id}, "
                "this may indicate a bug in query tracking"
            )
            raise ValueError(f"Query tracking error for {peer.name}")

        # Send the query
        pane.send_keys(text, enter=True)
        pane.send_keys("", enter=True)  # Extra Enter for robustness when peer is busy

        try:
            response = await asyncio.wait_for(future, timeout=timeout)
            return response
        except asyncio.TimeoutError:
            raise TimeoutError(f"No response from {peer.name} within {timeout}s")
        finally:
            # Cleanup pending file
            if pending_file.exists():
                pending_file.unlink()
            # QueryTracker cleanup is handled by caller or resolve

    def resolve_query(self, correlation_id: str, response: str) -> bool:
        """Resolve a pending query with a response (called by hooks).

        This method delegates to QueryTracker if available.

        Args:
            correlation_id: The correlation ID of the query
            response: The response text

        Returns:
            True if the query was found and resolved
        """
        if self._query_tracker:
            return self._query_tracker.resolve_query(correlation_id, response)
        logger.warning(f"No QueryTracker available to resolve {correlation_id}")
        return False

    async def cancel_queries_to_peer(self, peer_name: str) -> int:
        """Cancel all pending queries to a peer (called when peer disconnects).

        This method delegates to QueryTracker if available and also
        cleans up pending files.

        Args:
            peer_name: Name of the peer that disconnected

        Returns:
            Number of queries cancelled
        """
        cancelled = 0

        # Cancel via QueryTracker if available
        if self._query_tracker:
            cancelled = self._query_tracker.cancel_queries_to_peer_by_name(peer_name)

        # Also clean up pending files for this peer
        for pending_file in self._pending_dir.glob("*.json"):
            try:
                data = json.loads(pending_file.read_text())
                if data.get("to_peer") == peer_name:
                    pending_file.unlink()
            except json.JSONDecodeError as e:
                logger.debug(f"Skipping corrupted file {pending_file}: {e}")
                continue
            except OSError as e:
                logger.debug(f"Cannot read file {pending_file}: {e}")
                continue

        return cancelled

    def get_peer_status(self, peer: PeerConfig) -> PeerStatus:
        """Check if peer's tmux session is active."""
        if not peer.tmux_session:
            return PeerStatus.OFFLINE

        try:
            session_name, window_name = self._parse_tmux_target(peer.tmux_session)
            if not self._server:
                self._server = libtmux.Server()

            session = self._server.sessions.get(session_name=session_name)
            if session is None:
                return PeerStatus.OFFLINE

            if window_name:
                window = session.windows.get(window_name=window_name)
                if window is None:
                    return PeerStatus.OFFLINE

            return PeerStatus.ONLINE
        except (libtmux.exc.LibTmuxException, libtmux.exc.ObjectDoesNotExist):
            return PeerStatus.OFFLINE

    def install(self, dev: bool = False, **kwargs) -> None:
        """Install Claude Code hooks."""
        install_hooks(dev=dev)

    def uninstall(self, **kwargs) -> None:
        """Uninstall Claude Code hooks."""
        uninstall_hooks()

    def check_installed(self, **kwargs) -> bool:
        """Check if Claude Code hooks are installed."""
        return check_hooks_installed()

    def derive_circle(self, peer: PeerConfig) -> str:
        """Derive circle from tmux session name.

        For claudemux backend, the circle defaults to the tmux session name
        (the part before the colon in 'session:window').

        Args:
            peer: The peer configuration

        Returns:
            Circle name (tmux session name or "global" if no session)
        """
        if peer.tmux_session:
            session_name, _ = self._parse_tmux_target(peer.tmux_session)
            return session_name
        return "global"

    def _parse_tmux_target(self, tmux_target: str) -> tuple[str, str | None]:
        """Parse 'session:window' or 'session' format."""
        if ":" in tmux_target:
            session, window = tmux_target.split(":", 1)
            return session, window
        return tmux_target, None

    def _tmux_to_filename(self, tmux_session: str) -> str:
        """Convert tmux session:window to safe filename.

        Deprecated: Use _peer_id_to_filename instead.
        """
        return tmux_session.replace(":", "_").replace("/", "_")

    def _peer_id_to_filename(self, peer_id: str) -> str:
        """Convert peer_id to safe filename.

        For claudemux, peer_id is the tmux pane ID (e.g., "%42").
        The % is replaced with "pane_" for filesystem safety.
        """
        if peer_id.startswith("%"):
            return f"pane_{peer_id[1:]}"
        # Legacy format or other backends
        return peer_id.replace(":", "_").replace("/", "_").replace("%", "pane_")

    def _get_pane(self, tmux_target: str | None) -> libtmux.Pane | None:
        """Get the tmux pane for a target."""
        if not tmux_target:
            return None

        try:
            if not self._server:
                self._server = libtmux.Server()

            session_name, window_name = self._parse_tmux_target(tmux_target)
            session = self._server.sessions.get(session_name=session_name)
            if session is None:
                return None

            if window_name:
                window = session.windows.get(window_name=window_name)
                if window is None:
                    return None
                return window.active_pane

            return session.active_pane
        except libtmux.exc.LibTmuxException:
            return None
