"""Claudemux backend - tmux-based message delivery for Claude Code."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import TYPE_CHECKING
from uuid import uuid4

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


class ClaudemuxBackend(Backend):
    """Backend for Claude Code sessions running in tmux."""

    name = "claudemux"

    def __init__(self) -> None:
        self._server: libtmux.Server | None = None
        self._pending_queries: dict[str, asyncio.Future[str]] = {}
        self._pending_dir = Path.home() / ".repowire" / "pending"

    async def start(self) -> None:
        """Initialize tmux server connection."""
        self._server = libtmux.Server()
        self._pending_dir.mkdir(parents=True, exist_ok=True)

    async def stop(self) -> None:
        """Cleanup pending queries."""
        for future in self._pending_queries.values():
            if not future.done():
                future.cancel()
        self._pending_queries.clear()
        self._server = None

    async def send_message(self, peer: "PeerConfig", text: str) -> None:
        """Send a fire-and-forget message to a peer's tmux pane."""
        pane = self._get_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer.name}")

        pane.send_keys(text, enter=True)

    async def send_query(self, peer: "PeerConfig", text: str, timeout: float = 120.0) -> str:
        """Send a query and wait for response via hook callback."""
        pane = self._get_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer.name}")

        correlation_id = str(uuid4())

        # Create future for response
        response_future: asyncio.Future[str] = asyncio.Future()
        self._pending_queries[correlation_id] = response_future

        # Store correlation_id in pending file for hook to find
        # File is named by tmux session (sanitized) so stop_handler can find it
        pending_filename = self._tmux_to_filename(peer.tmux_session)
        pending_file = self._pending_dir / f"{pending_filename}.json"
        import json
        from datetime import datetime

        pending_data = {
            "correlation_id": correlation_id,
            "to_peer": peer.name,
            "query": text,
            "timestamp": datetime.utcnow().isoformat(),
        }
        pending_file.write_text(json.dumps(pending_data))

        # Send the query
        pane.send_keys(text, enter=True)

        try:
            response = await asyncio.wait_for(response_future, timeout=timeout)
            return response
        except asyncio.TimeoutError:
            raise TimeoutError(f"No response from {peer.name} within {timeout}s")
        finally:
            self._pending_queries.pop(correlation_id, None)
            if pending_file.exists():
                pending_file.unlink()

    def resolve_query(self, correlation_id: str, response: str) -> bool:
        """Resolve a pending query with a response (called by hooks).

        Args:
            correlation_id: The correlation ID of the query
            response: The response text

        Returns:
            True if the query was found and resolved
        """
        future = self._pending_queries.get(correlation_id)
        if future and not future.done():
            future.set_result(response)
            return True
        return False

    def get_peer_status(self, peer: "PeerConfig") -> PeerStatus:
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
        except Exception:
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

    def _parse_tmux_target(self, tmux_target: str) -> tuple[str, str | None]:
        """Parse 'session:window' or 'session' format."""
        if ":" in tmux_target:
            session, window = tmux_target.split(":", 1)
            return session, window
        return tmux_target, None

    def _tmux_to_filename(self, tmux_session: str) -> str:
        """Convert tmux session:window to safe filename."""
        return tmux_session.replace(":", "_").replace("/", "_")

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
        except Exception:
            return None
