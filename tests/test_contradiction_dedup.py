"""Tests for fail-loud contradiction events + transition-only dedup."""

from __future__ import annotations

from datetime import datetime, timezone

from repowire.config.models import AgentType, Config
from repowire.daemon import diagnostics as diag
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerStatus


def _registry(transport: WebSocketTransport) -> PeerRegistry:
    from unittest.mock import MagicMock

    registry = PeerRegistry(
        config=Config(),
        message_router=MagicMock(),
        query_tracker=None,
        transport=transport,
        ask_tracker=None,
    )
    registry._events.clear()
    return registry


def _online_no_pane_peer(peer_id: str = "repow-dev-abc12345") -> Peer:
    return Peer(
        peer_id=peer_id,
        display_name="ghost",
        path="/tmp/ghost",
        machine="test",
        backend=AgentType.CLAUDE_CODE,
        circle="dev",
        status=PeerStatus.ONLINE,
        pane_id=None,  # no pane => goes straight into disconnected candidates
        last_seen=datetime.now(timezone.utc),
    )


def _contradiction_events(registry: PeerRegistry) -> list[dict]:
    return [e for e in registry.get_events() if e.get("type") == "peer_contradiction"]


class FakeTransport(WebSocketTransport):
    """WebSocketTransport with controllable is_connected."""

    def __init__(self) -> None:
        super().__init__()
        self._fake_connected: set[str] = set()

    def is_connected(self, session_id: str) -> bool:
        return session_id in self._fake_connected


async def test_emits_once_per_transition():
    transport = FakeTransport()
    registry = _registry(transport)
    peer = _online_no_pane_peer()
    await registry.register_peer(peer)
    # Re-assert ONLINE (register may flip status); transport reports disconnected.
    (await registry.get_peer(peer.peer_id)).status = PeerStatus.ONLINE

    await registry._demote_disconnected_peers()
    events = _contradiction_events(registry)
    assert len(events) == 1
    assert events[0]["code"] == diag.ONLINE_BUT_NO_WS
    assert events[0]["severity"] == diag.SEVERITY_ERROR
    assert events[0]["peer_id"] == peer.peer_id

    # Second pass while still contradictory must NOT re-emit.
    (await registry.get_peer(peer.peer_id)).status = PeerStatus.ONLINE
    await registry._demote_disconnected_peers()
    assert len(_contradiction_events(registry)) == 1


async def test_recovery_then_recurrence_reemits():
    transport = FakeTransport()
    registry = _registry(transport)
    peer = _online_no_pane_peer()
    await registry.register_peer(peer)
    (await registry.get_peer(peer.peer_id)).status = PeerStatus.ONLINE

    await registry._demote_disconnected_peers()
    assert len(_contradiction_events(registry)) == 1

    # Recover: transport now reports connected -> clears the dedup entry.
    transport._fake_connected.add(peer.peer_id)
    await registry._demote_disconnected_peers()
    assert len(_contradiction_events(registry)) == 1  # no new event on recovery

    # Break again: disconnect + re-assert ONLINE -> should re-emit once.
    transport._fake_connected.discard(peer.peer_id)
    (await registry.get_peer(peer.peer_id)).status = PeerStatus.ONLINE
    await registry._demote_disconnected_peers()
    assert len(_contradiction_events(registry)) == 2


def test_clear_all_contradictions():
    transport = FakeTransport()
    registry = _registry(transport)
    peer1 = _online_no_pane_peer("p1")
    peer2 = _online_no_pane_peer("p2")
    registry._emit_contradiction(peer1, diag.ONLINE_BUT_NO_WS, "error", "one")
    registry._emit_contradiction(peer1, diag.PANE_MISSING, "error", "two")
    registry._emit_contradiction(peer2, diag.ONLINE_BUT_NO_WS, "error", "three")
    registry._clear_all_contradictions("p1")

    registry._emit_contradiction(peer1, diag.ONLINE_BUT_NO_WS, "error", "again")
    registry._emit_contradiction(peer2, diag.ONLINE_BUT_NO_WS, "error", "again")
    events = _contradiction_events(registry)
    assert [e["peer_id"] for e in events].count("p1") == 3
    assert [e["peer_id"] for e in events].count("p2") == 1
