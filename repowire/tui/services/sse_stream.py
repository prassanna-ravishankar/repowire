"""SSE stream client for real-time events."""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncGenerator
from typing import Any

import httpx

logger = logging.getLogger(__name__)


class SSEStream:
    """Server-Sent Events stream for real-time updates."""

    def __init__(self, base_url: str = "http://127.0.0.1:8377") -> None:
        self.base_url = base_url
        self._running = False

    async def stream_events(self) -> AsyncGenerator[dict[str, Any], None]:
        """Stream events from the daemon.

        Yields:
            Event dictionaries as they arrive
        """
        self._running = True
        url = f"{self.base_url}/events/stream"

        while self._running:
            try:
                async with httpx.AsyncClient(timeout=None) as client:
                    async with client.stream("GET", url) as response:
                        async for line in response.aiter_lines():
                            if not self._running:
                                break
                            if line.startswith("data: "):
                                data = line[6:]
                                try:
                                    event = json.loads(data)
                                    yield event
                                except json.JSONDecodeError as e:
                                    logger.warning(f"Malformed SSE event, skipping: {e}")
                                    continue
            except httpx.RequestError as e:
                if self._running:
                    logger.warning(f"SSE connection lost ({type(e).__name__}), reconnecting...")
                    await asyncio.sleep(2)  # Reconnect delay
            except asyncio.CancelledError:
                break

    def stop(self) -> None:
        """Stop the event stream."""
        self._running = False
