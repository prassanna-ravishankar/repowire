"""Tests for ws-hook lifetime binding to its owning agent process."""

from __future__ import annotations

import pytest

from repowire.hooks import websocket_hook


@pytest.mark.asyncio
async def test_agent_lifetime_watchdog_exits_when_agent_pid_disappears(monkeypatch):
    async def fake_sleep(_seconds: float) -> None:
        return None

    monkeypatch.setattr(websocket_hook.asyncio, "sleep", fake_sleep)
    monkeypatch.setattr(websocket_hook, "_pid_exists", lambda _pid: False)

    with pytest.raises(websocket_hook.AgentExitedError):
        await websocket_hook._watch_agent_lifetime(12345, "%7")
