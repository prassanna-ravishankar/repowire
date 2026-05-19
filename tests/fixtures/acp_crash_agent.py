"""ACP agent stub that exits hard on the second prompt.

Used to exercise the "subprocess died mid-prompt" branch of the broker: the
manager should drop the cached client so the next ask respawns a fresh one.

First prompt: returns a normal echo response.
Second prompt: ``sys.exit(1)`` before responding — the client side sees the
stdio close mid-request.
"""

from __future__ import annotations

import asyncio
import sys
import uuid
from typing import Any

import acp
from acp.schema import (
    AgentCapabilities,
    AgentMessageChunk,
    TextContentBlock,
)


class CrashOnSecondPromptAgent:
    def __init__(self) -> None:
        self._conn: Any = None
        self._prompt_count = 0

    def on_connect(self, conn: Any) -> None:
        self._conn = conn

    async def initialize(self, protocol_version: int, **_: Any) -> acp.InitializeResponse:
        return acp.InitializeResponse(
            protocol_version=protocol_version,
            agent_capabilities=AgentCapabilities(),
        )

    async def new_session(self, cwd: str, **_: Any) -> acp.NewSessionResponse:
        del cwd
        return acp.NewSessionResponse(session_id=f"crash-{uuid.uuid4().hex[:8]}")

    async def prompt(
        self,
        prompt: list[Any],
        session_id: str,
        **_: Any,
    ) -> acp.PromptResponse:
        self._prompt_count += 1
        if self._prompt_count >= 2:
            sys.stderr.write("crash agent: exiting on second prompt\n")
            sys.stderr.flush()
            sys.exit(1)
        body = " ".join(getattr(b, "text", "") for b in prompt)
        if self._conn is not None:
            await self._conn.session_update(
                session_id=session_id,
                update=AgentMessageChunk(
                    session_update="agent_message_chunk",
                    content=TextContentBlock(type="text", text=f"[ok] {body}"),
                ),
            )
        return acp.PromptResponse(stop_reason="end_turn")

    async def cancel(self, session_id: str, **_: Any) -> None:
        del session_id


async def main() -> None:
    await acp.run_agent(CrashOnSecondPromptAgent())  # type: ignore[arg-type]


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except (KeyboardInterrupt, BrokenPipeError):
        sys.exit(0)
