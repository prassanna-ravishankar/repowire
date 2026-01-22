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
