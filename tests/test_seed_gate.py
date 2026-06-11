"""Tests for the shared seed-settle gate (repowire.daemon.seed_gate).

The same bounded wait is consumed by the queued-notify flush
(daemon/routes/websocket.py) and by live ask/notify delivery
(daemon/peer_delivery.py).
"""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from repowire.daemon import seed_gate
from repowire.daemon.seed_gate import await_seed_settled


def _registry(peers_by_call: list[object]):
    """Fake registry whose get_peer returns the next snapshot per call."""
    calls = {"n": 0}

    async def get_peer(_session_id: str):
        idx = min(calls["n"], len(peers_by_call) - 1)
        calls["n"] += 1
        return peers_by_call[idx]

    return SimpleNamespace(get_peer=get_peer)


async def test_no_wait_when_not_pending_first_turn():
    peer = SimpleNamespace(turn_state="idle", display_name="p")
    reg = _registry([peer])
    result = await await_seed_settled("sess", reg)
    assert result is peer


async def test_no_wait_when_peer_missing():
    reg = _registry([None])
    result = await await_seed_settled("sess", reg)
    assert result is None


async def test_waits_until_pending_first_turn_clears():
    pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
    settled = SimpleNamespace(turn_state="working", display_name="p")
    reg = _registry([pending, pending, settled])

    with patch.object(seed_gate.asyncio, "sleep", new_callable=AsyncMock) as sleep:
        result = await await_seed_settled("sess", reg)

    assert result is settled
    assert sleep.await_count >= 1  # polled at least once while pending


async def test_proceeds_on_timeout_while_still_pending():
    pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
    reg = _registry([pending])

    # Zero wait budget: the deadline check trips on the first iteration, so the
    # loop breaks and proceeds anyway rather than hanging forever.
    with (
        patch.object(seed_gate.asyncio, "sleep", new_callable=AsyncMock),
        patch.object(seed_gate, "SEED_SETTLE_WAIT_SECONDS", 0.0),
    ):
        result = await await_seed_settled("sess", reg)

    assert result is pending


async def test_returns_none_when_peer_vanishes_mid_wait():
    pending = SimpleNamespace(turn_state="pending_first_turn", display_name="p")
    reg = _registry([pending, None])

    with patch.object(seed_gate.asyncio, "sleep", new_callable=AsyncMock):
        result = await await_seed_settled("sess", reg)

    assert result is None
