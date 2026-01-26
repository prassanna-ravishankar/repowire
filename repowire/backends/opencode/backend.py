"""OpenCode backend - WebSocket-based message delivery for OpenCode sessions."""

from __future__ import annotations

from typing import TYPE_CHECKING

from repowire.backends.base import Backend
from repowire.backends.opencode.installer import (
    check_plugin_installed,
    install_plugin,
    uninstall_plugin,
)
from repowire.protocol.peers import PeerStatus

if TYPE_CHECKING:
    from repowire.config.models import PeerConfig
    from repowire.daemon.websocket_manager import WebSocketManager


class OpencodeBackend(Backend):
    """Backend for OpenCode sessions using WebSocket plugin connections.

    Unlike claudemux which uses tmux for message delivery, the OpenCode backend
    relies on a TypeScript plugin that:
    1. Connects to the daemon via WebSocket on startup
    2. Receives queries and injects them into the user's session
    3. Sends responses back via WebSocket

    No opencode-ai SDK is needed - all communication flows through WebSocket.
    """

    name = "opencode"

    def __init__(self, ws_manager: WebSocketManager | None = None) -> None:
        """Initialize OpenCode backend.

        Args:
            ws_manager: WebSocket manager instance. If None, will use global singleton.
        """
        self._ws_manager = ws_manager

    def _get_ws_manager(self) -> WebSocketManager:
        """Get the WebSocket manager, using injected instance or global singleton."""
        if self._ws_manager is not None:
            return self._ws_manager
        from repowire.daemon.websocket_manager import get_ws_manager

        return get_ws_manager()

    async def start(self) -> None:
        """Initialize backend."""
        pass

    async def stop(self) -> None:
        """Cleanup."""
        pass

    async def send_message(self, peer: PeerConfig, text: str) -> None:
        """Send a fire-and-forget message to a peer via WebSocket.

        Args:
            peer: Peer configuration
            text: Message text
        """
        ws_manager = self._get_ws_manager()
        if not ws_manager.is_connected(peer.name):
            raise ValueError(f"Peer {peer.name} is not connected via WebSocket")

        # Send as notification (fire-and-forget)
        success = await ws_manager.send_notification("daemon", peer.name, text)
        if not success:
            raise ValueError(f"Failed to send notification to {peer.name}")

    async def send_query(self, peer: PeerConfig, text: str, timeout: float = 120.0) -> str:
        """Send a query and get response via WebSocket.

        The plugin receives the query, injects it into the user's session using
        the OpenCode SDK's session.prompt(), and returns the response via WebSocket.

        Args:
            peer: Peer configuration
            text: Query text
            timeout: Timeout in seconds

        Returns:
            Response text from the peer
        """
        ws_manager = self._get_ws_manager()
        if not ws_manager.is_connected(peer.name):
            raise ValueError(f"Peer {peer.name} is not connected via WebSocket")

        # Send query and wait for response
        response = await ws_manager.send_query("daemon", peer.name, text, timeout)
        return response

    def get_peer_status(self, peer: PeerConfig) -> PeerStatus:
        """Check if peer is connected via WebSocket."""
        ws_manager = self._get_ws_manager()
        return ws_manager.get_peer_status(peer.name)

    def install(self, global_install: bool = True, **kwargs) -> None:
        """Install OpenCode plugin."""
        install_plugin(global_install=global_install)

    def uninstall(self, global_install: bool = True, **kwargs) -> None:
        """Uninstall OpenCode plugin."""
        uninstall_plugin(global_install=global_install)

    def check_installed(self, global_install: bool = True, **kwargs) -> bool:
        """Check if OpenCode plugin is installed."""
        return check_plugin_installed(global_install=global_install)

    async def cancel_queries_to_peer(self, peer_name: str) -> int:
        """Cancel pending queries to a peer.

        Args:
            peer_name: Name of the peer

        Returns:
            Number of queries cancelled
        """
        ws_manager = self._get_ws_manager()
        return await ws_manager.cancel_queries_to_peer(peer_name)
