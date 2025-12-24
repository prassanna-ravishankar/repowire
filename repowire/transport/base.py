from __future__ import annotations

from abc import ABC, abstractmethod


class Transport(ABC):
    @abstractmethod
    async def send_message(self, session_id: str, text: str) -> str:
        """Send a message and return the response."""
        pass

    @abstractmethod
    async def send_notification(self, session_id: str, text: str) -> None:
        """Send a fire-and-forget notification."""
        pass

    @abstractmethod
    async def close(self) -> None:
        """Close the transport connection."""
        pass
