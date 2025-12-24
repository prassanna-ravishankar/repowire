from __future__ import annotations

from typing import Any

from opencode_ai import AsyncOpencode

from repowire.transport.base import Transport


class OpenCodeTransport(Transport):
    def __init__(self, base_url: str | None = None) -> None:
        self.client = AsyncOpencode(base_url=base_url)

    async def send_message(self, session_id: str, text: str) -> str:
        """Send query to OpenCode session and wait for response."""
        response = await self.client.session.chat(
            session_id,
            parts=[{"type": "text", "text": text}],
        )
        return self._extract_response_text(response)

    async def send_notification(self, session_id: str, text: str) -> None:
        """Send notification (fire-and-forget)."""
        await self.client.session.chat(
            session_id,
            parts=[{"type": "text", "text": text}],
        )

    def _extract_response_text(self, response: Any) -> str:
        if hasattr(response, "parts"):
            for part in response.parts:
                if hasattr(part, "text"):
                    return part.text
        if hasattr(response, "text"):
            return response.text
        return str(response)

    async def close(self) -> None:
        await self.client.close()
