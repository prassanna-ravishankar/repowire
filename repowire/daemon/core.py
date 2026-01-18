"""Core logic for the Repowire daemon."""

from __future__ import annotations

import asyncio
import socket
from datetime import datetime
from typing import TYPE_CHECKING

from repowire.config.models import Config, PeerConfig, load_config
from repowire.protocol.peers import Peer, PeerStatus

if TYPE_CHECKING:
    from repowire.backends.base import Backend


class PeerManager:
    """Manages peer registry and message routing.

    Thread-safe with asyncio locks. Delegates actual message
    delivery to the configured backend.
    """

    def __init__(self, backend: Backend, config: Config | None = None) -> None:
        self._backend = backend
        self._config = config or load_config()
        self._peers: dict[str, Peer] = {}
        self._lock = asyncio.Lock()
        self._machine = socket.gethostname()

    @property
    def backend_name(self) -> str:
        """Get the backend name."""
        return self._backend.name

    async def start(self) -> None:
        """Start the peer manager and backend."""
        await self._backend.start()

    async def stop(self) -> None:
        """Stop the peer manager and backend."""
        await self._backend.stop()

    async def register_peer(self, peer: Peer) -> None:
        """Register a peer in the mesh."""
        async with self._lock:
            peer.status = PeerStatus.ONLINE
            peer.last_seen = datetime.utcnow()
            self._peers[peer.name] = peer

    async def unregister_peer(self, name: str) -> bool:
        """Unregister a peer from the mesh."""
        async with self._lock:
            if name in self._peers:
                del self._peers[name]
                return True
            return False

    async def get_peer(self, name: str) -> Peer | None:
        """Get a peer by name."""
        async with self._lock:
            return self._peers.get(name)

    async def get_all_peers(self) -> list[Peer]:
        """Get all registered peers."""
        # Reload config for fresh peer info
        self._config = load_config()

        async with self._lock:
            local_peers = {p.name: p for p in self._peers.values()}

        # Build peers from config with backend status
        result: list[Peer] = []
        seen = set(local_peers.keys())

        for peer_config in self._config.peers.values():
            if peer_config.name in seen:
                # Use locally registered peer
                result.append(local_peers[peer_config.name])
            else:
                # Build peer from config
                status = self._backend.get_peer_status(peer_config)
                result.append(
                    Peer(
                        name=peer_config.name,
                        path=peer_config.path,
                        machine=self._machine,
                        tmux_session=peer_config.tmux_session,
                        status=status,
                        last_seen=datetime.utcnow() if status != PeerStatus.OFFLINE else None,
                        metadata=peer_config.metadata,
                    )
                )
                seen.add(peer_config.name)

        # Add any locally registered peers not in config
        for name, peer in local_peers.items():
            if name not in seen:
                result.append(peer)

        return result

    async def update_peer_status(self, name: str, status: PeerStatus) -> bool:
        """Update peer status."""
        async with self._lock:
            if name in self._peers:
                self._peers[name].status = status
                self._peers[name].last_seen = datetime.utcnow()
                return True
            return False

    def _get_peer_config(self, name: str) -> PeerConfig | None:
        """Get peer config by name."""
        self._config = load_config()
        return self._config.get_peer(name)

    async def query(
        self,
        from_peer: str,
        to_peer: str,
        text: str,
        timeout: float = 120.0,
    ) -> str:
        """Send a query to a peer and wait for response.

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Query text
            timeout: Timeout in seconds

        Returns:
            Response text from the peer

        Raises:
            ValueError: If peer not found
            TimeoutError: If no response within timeout
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check backend-specific requirements
        if self._backend.name == "claudemux" and not peer_config.tmux_session:
            raise ValueError(f"Peer {to_peer} has no tmux session (required for claudemux backend)")
        elif self._backend.name == "opencode" and not peer_config.opencode_url:
            raise ValueError(f"Peer {to_peer} has no opencode_url (required for opencode backend)")

        # Format the query with sender info
        formatted_query = f"@{from_peer} asks: {text}"

        return await self._backend.send_query(peer_config, formatted_query, timeout)

    async def notify(self, from_peer: str, to_peer: str, text: str) -> bool:
        """Send a notification to a peer (fire-and-forget).

        Args:
            from_peer: Name of the sending peer
            to_peer: Name of the target peer
            text: Notification text

        Returns:
            True if message was sent successfully
        """
        peer_config = self._get_peer_config(to_peer)
        if not peer_config:
            raise ValueError(f"Unknown peer: {to_peer}")

        # Check backend-specific requirements
        if self._backend.name == "claudemux" and not peer_config.tmux_session:
            raise ValueError(f"Peer {to_peer} has no tmux session (required for claudemux backend)")
        elif self._backend.name == "opencode" and not peer_config.opencode_url:
            raise ValueError(f"Peer {to_peer} has no opencode_url (required for opencode backend)")

        # Format the notification with sender info
        formatted_message = f"@{from_peer} says: {text}"

        try:
            await self._backend.send_message(peer_config, formatted_message)
            return True
        except Exception:
            return False

    async def broadcast(
        self,
        from_peer: str,
        text: str,
        exclude: list[str] | None = None,
    ) -> list[str]:
        """Broadcast a message to all peers.

        Args:
            from_peer: Name of the sending peer
            text: Broadcast text
            exclude: Peer names to exclude

        Returns:
            List of peer names that received the message
        """
        exclude = set(exclude or [])
        exclude.add(from_peer)  # Don't send to self

        # Reload config
        self._config = load_config()
        sent_to: list[str] = []

        formatted_message = f"@{from_peer} broadcasts: {text}"

        for peer_config in self._config.peers.values():
            if peer_config.name in exclude:
                continue

            # Check backend-specific requirements
            if self._backend.name == "claudemux" and not peer_config.tmux_session:
                continue
            elif self._backend.name == "opencode" and not peer_config.opencode_url:
                continue

            status = self._backend.get_peer_status(peer_config)
            if status == PeerStatus.OFFLINE:
                continue

            try:
                await self._backend.send_message(peer_config, formatted_message)
                sent_to.append(peer_config.name)
            except Exception:
                # Log but don't fail the broadcast
                pass

        return sent_to
