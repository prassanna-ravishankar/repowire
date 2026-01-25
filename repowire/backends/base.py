"""Base backend interface for message delivery."""

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import TYPE_CHECKING

from repowire.protocol.peers import PeerStatus

if TYPE_CHECKING:
    from repowire.config.models import PeerConfig


class Backend(ABC):
    """Abstract base class for message delivery backends."""

    name: str

    @abstractmethod
    async def start(self) -> None:
        """Initialize backend resources."""
        pass

    @abstractmethod
    async def stop(self) -> None:
        """Cleanup backend resources."""
        pass

    @abstractmethod
    async def send_message(self, peer: PeerConfig, text: str) -> None:
        """Fire-and-forget message to peer."""
        pass

    @abstractmethod
    async def send_query(self, peer: PeerConfig, text: str, timeout: float = 120.0) -> str:
        """Send query and wait for response."""
        pass

    @abstractmethod
    def get_peer_status(self, peer: PeerConfig) -> PeerStatus:
        """Check if peer is online."""
        pass

    def install(self, **kwargs) -> None:
        """Install platform-specific integrations."""
        raise NotImplementedError(f"{self.name} has no installer")

    def uninstall(self, **kwargs) -> None:
        """Uninstall integrations."""
        raise NotImplementedError(f"{self.name} has no uninstaller")

    def check_installed(self, **kwargs) -> bool:
        """Check if integrations are installed."""
        return False

    def derive_circle(self, peer: PeerConfig) -> str:
        """Derive circle from peer config. Override in subclasses.

        Args:
            peer: The peer configuration

        Returns:
            Circle name (default: "global")
        """
        return "global"

    async def cancel_queries_to_peer(self, peer_name: str) -> int:
        """Cancel pending queries to a peer.

        Args:
            peer_name: Name of the peer

        Returns:
            Number of queries cancelled
        """
        return 0

    def resolve_query(self, correlation_id: str, response: str) -> bool:
        """Resolve a pending query with a response.

        Args:
            correlation_id: The correlation ID of the pending query
            response: The response text

        Returns:
            True if the query was found and resolved
        """
        return False
