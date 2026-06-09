"""Tests for ws-hook lifetime binding to its owning agent process."""

from __future__ import annotations

import json
from unittest.mock import AsyncMock, patch

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


@pytest.mark.asyncio
async def test_dead_agent_pid_before_connect_posts_offline_and_exits(monkeypatch):
    """A hook whose agent died while disconnected must tell the daemon and
    exit instead of reconnecting forever."""
    monkeypatch.setenv("TMUX_PANE", "%7")
    monkeypatch.setenv("REPOWIRE_AGENT_PID", "12345")
    monkeypatch.setenv("REPOWIRE_PEER_ID", "repow-test-deadbeef")
    monkeypatch.setattr(websocket_hook, "_pid_exists", lambda _pid: False)
    # Keep startup baseline capture off the real system.
    monkeypatch.setattr(websocket_hook, "_get_pane_pid", lambda _pane: None)
    monkeypatch.setattr(websocket_hook, "_get_pane_command", lambda _pane: None)

    posted: list[tuple[str, dict]] = []
    monkeypatch.setattr(
        websocket_hook,
        "daemon_post",
        lambda path, payload: posted.append((path, payload)) or {},
    )
    cleared: list[str] = []
    monkeypatch.setattr(
        websocket_hook, "clear_pane_runtime_state", lambda pane: cleared.append(pane)
    )

    assert await websocket_hook.main() == 0

    assert cleared == ["%7"]
    assert len(posted) == 1
    path, payload = posted[0]
    assert path == "/peers/repow-test-deadbeef/offline"
    assert payload["reason"] == "agent_exited"
    assert payload["source"] == "ws_hook"
    assert payload["terminal"] is True


class TestInconclusivePing:
    @pytest.mark.asyncio
    async def test_inconclusive_ping_omits_pane_alive_and_does_not_strike(self):
        websocket_hook._consecutive_ping_unsafe = 0
        ws = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=None),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
        ):
            await websocket_hook.handle_message({"type": "ping"}, "%5", ws)

        sent = json.loads(ws.send.call_args.args[0])
        assert sent["type"] == "pong"
        assert "pane_alive" not in sent
        assert websocket_hook._consecutive_ping_unsafe == 0

    @pytest.mark.asyncio
    async def test_inconclusive_ping_preserves_existing_strikes(self):
        websocket_hook._consecutive_ping_unsafe = 2
        ws = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=None),
            patch(
                "repowire.hooks.websocket_hook.get_tmux_info",
                return_value={"session_name": "0"},
            ),
        ):
            await websocket_hook.handle_message({"type": "ping"}, "%5", ws)
        assert websocket_hook._consecutive_ping_unsafe == 2
        websocket_hook._consecutive_ping_unsafe = 0


class TestInconclusiveInjection:
    @pytest.mark.asyncio
    async def test_ask_with_inconclusive_safety_fails_loud_without_injection(self):
        """An inconclusive pane check must fail the delivery (no injection into
        a possibly-bare shell) without killing the hook."""
        ws = AsyncMock()
        with (
            patch("repowire.hooks.websocket_hook._is_pane_safe", return_value=None),
            patch("repowire.hooks.websocket_hook._tmux_send_keys") as mock_send,
        ):
            await websocket_hook.handle_message(
                {
                    "type": "ask",
                    "delivery_id": "ask-delivery-9",
                    "correlation_id": "ask-xyz",
                    "from_peer": "alice",
                    "text": "ping?",
                },
                "%5",
                ws,
            )

        mock_send.assert_not_called()
        frames = [json.loads(c.args[0]) for c in ws.send.call_args_list]
        ack = next(f for f in frames if f["type"] == "delivery_ack")
        assert ack["status"] == "failed"
        assert "inconclusive" in ack["detail"]
        err = next(f for f in frames if f["type"] == "error")
        assert err["correlation_id"] == "ask-xyz"
