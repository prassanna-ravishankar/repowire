"""OpenCode backend - SDK-based message delivery for OpenCode AI."""

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


class OpencodeBackend(Backend):
    """Backend for OpenCode AI sessions using the opencode-ai SDK."""

    name = "opencode"

    def __init__(self) -> None:
        self._clients: dict[str, object] = {}  # peer_name -> AsyncOpencode client

    async def start(self) -> None:
        """Initialize backend."""
        pass

    async def stop(self) -> None:
        """Cleanup clients."""
        self._clients.clear()

    async def send_message(self, peer: PeerConfig, text: str) -> None:
        """Send a fire-and-forget message to a peer via OpenCode SDK."""
        client, session_id = await self._get_client(peer)
        if not client or not session_id:
            raise ValueError(f"Could not connect to peer {peer.name}")

        # Use SDK to send message (no_reply=True for fire-and-forget)
        await client.session.prompt(
            id=session_id,
            parts=[{"type": "text", "text": text}],
            no_reply=True,
        )

    async def send_query(self, peer: PeerConfig, text: str, timeout: float = 120.0) -> str:
        """Send a query and get response directly from SDK (no hooks needed)."""
        client, session_id = await self._get_client(peer)
        if not client or not session_id:
            raise ValueError(f"Could not connect to peer {peer.name}")

        # SDK returns response directly - no need for hooks!
        result = await client.session.prompt(
            id=session_id,
            parts=[{"type": "text", "text": text}],
        )

        # Extract text from response parts
        for part in result.parts:
            if hasattr(part, "text"):
                return part.text
        return ""

    def get_peer_status(self, peer: PeerConfig) -> PeerStatus:
        """Check if peer's OpenCode instance is reachable."""
        # For OpenCode, we check if the peer has an opencode_url configured
        if not peer.opencode_url:
            return PeerStatus.OFFLINE

        # Could do a health check here, but for now just assume online if URL exists
        return PeerStatus.ONLINE

    def install(self, global_install: bool = True, **kwargs) -> None:
        """Install OpenCode plugin."""
        install_plugin(global_install=global_install)

    def uninstall(self, global_install: bool = True, **kwargs) -> None:
        """Uninstall OpenCode plugin."""
        uninstall_plugin(global_install=global_install)

    def check_installed(self, global_install: bool = True, **kwargs) -> bool:
        """Check if OpenCode plugin is installed."""
        return check_plugin_installed(global_install=global_install)

    async def _get_client(self, peer: PeerConfig) -> tuple[object | None, str | None]:
        """Get or create an OpenCode client for a peer.

        Returns:
            Tuple of (client, session_id) or (None, None) if unable to connect.
        """
        opencode_url = peer.opencode_url
        session_id = peer.session_id

        if not opencode_url:
            return None, None

        if peer.name in self._clients:
            return self._clients[peer.name], session_id

        try:
            from opencode_ai import AsyncOpencode

            client = AsyncOpencode(base_url=opencode_url)
            self._clients[peer.name] = client
            return client, session_id
        except ImportError:
            # opencode-ai SDK not installed
            return None, None
        except Exception:  # TODO: Catch specific exceptions from opencode-ai SDK
            return None, None
