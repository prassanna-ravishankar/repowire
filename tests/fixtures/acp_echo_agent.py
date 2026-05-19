"""Minimal ACP-speaking agent used as a subprocess in integration tests.

Implements just enough of the ACP ``Agent`` interface to:
  * complete ``initialize``,
  * accept ``session/new``,
  * answer ``session/prompt`` with one streamed ``agent_message_chunk`` whose
    body echoes the original prompt text framed as ``[echo] <text>``,
  * report ``stop_reason=end_turn``.

That's enough to exercise the broker-side ACP client end-to-end without
shelling out to a real ``codex-acp`` (or any other adapter) — which is the
phase-3 integration test we want.
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


class EchoAgent:
    """ACP agent that echoes prompt text as one streamed agent_message_chunk.

    Implements the structural ``Agent`` protocol — duck-typed by the SDK's
    JSON-RPC router. Inheriting from ``acp.Agent`` is unnecessary and trips
    type-checker LSP rules because of the SDK's protocol-decorator dance.
    """

    def __init__(self) -> None:
        self._conn: Any = None

    def on_connect(self, conn: Any) -> None:
        self._conn = conn

    async def initialize(self, protocol_version: int, **_: Any) -> acp.InitializeResponse:
        return acp.InitializeResponse(
            protocol_version=protocol_version,
            agent_capabilities=AgentCapabilities(),
        )

    async def new_session(self, cwd: str, **_: Any) -> acp.NewSessionResponse:
        del cwd
        return acp.NewSessionResponse(session_id=f"echo-{uuid.uuid4().hex[:8]}")

    async def prompt(
        self,
        prompt: list[Any],
        session_id: str,
        **_: Any,
    ) -> acp.PromptResponse:
        body = _extract_text(prompt)
        reply = f"[echo] {body}"
        if self._conn is not None:
            await self._conn.session_update(
                session_id=session_id,
                update=AgentMessageChunk(
                    session_update="agent_message_chunk",
                    content=TextContentBlock(type="text", text=reply),
                ),
            )
        return acp.PromptResponse(stop_reason="end_turn")

    async def cancel(self, session_id: str, **_: Any) -> None:
        del session_id


def _extract_text(prompt: list[Any]) -> str:
    parts: list[str] = []
    for block in prompt:
        text = getattr(block, "text", None)
        if text:
            parts.append(text)
    return " ".join(parts)


async def main() -> None:
    await acp.run_agent(EchoAgent())  # type: ignore[arg-type]


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except (KeyboardInterrupt, BrokenPipeError):
        sys.exit(0)
