"""ACP agent stub that sleeps forever on prompt.

Used to exercise the prompt-timeout branch: the client should call
``session/cancel``, close the subprocess, and mark itself crashed so the
manager evicts it.
"""

from __future__ import annotations

import asyncio
import sys
import uuid
from typing import Any

import acp
from acp.schema import AgentCapabilities


class SlowAgent:
    def __init__(self) -> None:
        self._cancelled: dict[str, asyncio.Event] = {}

    async def initialize(self, protocol_version: int, **_: Any) -> acp.InitializeResponse:
        return acp.InitializeResponse(
            protocol_version=protocol_version,
            agent_capabilities=AgentCapabilities(),
        )

    async def new_session(self, cwd: str, **_: Any) -> acp.NewSessionResponse:
        del cwd
        sid = f"slow-{uuid.uuid4().hex[:8]}"
        self._cancelled[sid] = asyncio.Event()
        return acp.NewSessionResponse(session_id=sid)

    async def prompt(
        self,
        prompt: list[Any],
        session_id: str,
        **_: Any,
    ) -> acp.PromptResponse:
        del prompt
        event = self._cancelled.setdefault(session_id, asyncio.Event())
        try:
            await asyncio.wait_for(event.wait(), timeout=30.0)
            return acp.PromptResponse(stop_reason="cancelled")
        except asyncio.TimeoutError:
            return acp.PromptResponse(stop_reason="end_turn")

    async def cancel(self, session_id: str, **_: Any) -> None:
        event = self._cancelled.setdefault(session_id, asyncio.Event())
        event.set()


async def main() -> None:
    await acp.run_agent(SlowAgent())  # type: ignore[arg-type]


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except (KeyboardInterrupt, BrokenPipeError):
        sys.exit(0)
