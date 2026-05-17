"""Shared ACP probe client.

Spawns an ACP-compliant agent subprocess and exposes a thin async wrapper that
collects ``session/update`` notifications and permission requests for the
per-adapter probe modules to inspect. Built on the official ``acp`` SDK
(https://agentclientprotocol.github.io/python-sdk/), so this file only adds
the recording + scenario glue, not JSON-RPC plumbing.
"""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import acp


@dataclass
class RecordingClient(acp.Client):
    """ACP ``Client`` implementation that records every callback the agent makes.

    Probe modules read ``updates`` / ``permission_requests`` after a turn to
    assert on what the agent sent. Each ``updates`` entry is a re-assembled
    ``SessionNotification`` so probes can pattern-match on ``type(u.update)``.
    """

    updates: list[acp.SessionNotification] = field(default_factory=list)
    permission_requests: list[Any] = field(default_factory=list)
    permission_default: str = "allow"

    async def session_update(self, session_id: str, update: Any, **_: Any) -> None:
        self.updates.append(acp.SessionNotification(session_id=session_id, update=update))

    async def request_permission(
        self,
        options: list[Any],
        session_id: str,
        tool_call: Any,
        **_: Any,
    ) -> acp.RequestPermissionResponse:
        self.permission_requests.append(
            {"session_id": session_id, "options": options, "tool_call": tool_call}
        )
        chosen = next(
            (opt for opt in options if getattr(opt, "option_id", None) == self.permission_default),
            options[0] if options else None,
        )
        if chosen is None:
            return acp.RequestPermissionResponse(
                outcome={"outcome": "cancelled"}  # type: ignore[arg-type]
            )
        return acp.RequestPermissionResponse(
            outcome={"outcome": "selected", "option_id": chosen.option_id}  # type: ignore[arg-type]
        )

    async def read_text_file(
        self,
        path: str,
        session_id: str,
        limit: int | None = None,
        line: int | None = None,
        **_: Any,
    ) -> acp.ReadTextFileResponse:
        return acp.ReadTextFileResponse(content=Path(path).read_text())

    async def write_text_file(
        self, content: str, path: str, session_id: str, **_: Any
    ) -> acp.WriteTextFileResponse:
        Path(path).write_text(content)
        return acp.WriteTextFileResponse()


@dataclass
class ProbeResult:
    """One probe row's outcome. ``status`` is the matrix cell value."""

    capability: str
    status: str  # "pass" | "fail" | "partial" | "n/a"
    detail: str = ""
    raw: dict[str, Any] = field(default_factory=dict)


@asynccontextmanager
async def open_agent(
    command: str,
    *args: str,
    cwd: str | Path | None = None,
) -> AsyncIterator[tuple[acp.client.connection.ClientSideConnection, RecordingClient]]:
    """Spawn an ACP agent subprocess and yield ``(connection, recording_client)``.

    The connection is already wired through the SDK; the caller drives it with
    ``conn.initialize(...)``, ``conn.new_session(...)``, ``conn.prompt(...)`` etc.
    Tear-down happens on context exit.
    """
    recorder = RecordingClient()
    async with acp.spawn_agent_process(recorder, command, *args, cwd=cwd) as (conn, _proc):
        yield conn, recorder


async def initialize_probe(command: str, *args: str) -> ProbeResult:
    """C1: verify ``initialize`` handshake completes and ``agentCapabilities`` is populated."""
    try:
        async with open_agent(command, *args) as (conn, _rec):
            resp = await asyncio.wait_for(
                conn.initialize(protocol_version=acp.PROTOCOL_VERSION),
                timeout=10.0,
            )
            agent_caps = getattr(resp, "agent_capabilities", None)
            return ProbeResult(
                capability="C1",
                status="pass" if agent_caps is not None else "partial",
                detail=f"protocol_version={resp.protocol_version}",
                raw={"agent_capabilities": str(agent_caps)},
            )
    except Exception as e:  # noqa: BLE001 — probe surface, all failures are data
        return ProbeResult(capability="C1", status="fail", detail=f"{type(e).__name__}: {e}")


if __name__ == "__main__":
    import sys

    if len(sys.argv) < 2:
        print("usage: client.py <agent-command> [agent-args...]", file=sys.stderr)
        sys.exit(2)
    result = asyncio.run(initialize_probe(sys.argv[1], *sys.argv[2:]))
    print(f"{result.capability}: {result.status} — {result.detail}")
    if result.raw:
        for k, v in result.raw.items():
            print(f"  {k}: {v}")
