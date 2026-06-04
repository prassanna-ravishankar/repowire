"""Tests for peer-registry event seam helpers."""

from __future__ import annotations

from datetime import datetime, timezone

from repowire.agent_types import AgentType
from repowire.daemon.registry_events import PeerContradictionTracker
from repowire.protocol.peers import Peer, PeerStatus


def _peer(peer_id: str = "repow-dev-abc12345") -> Peer:
    return Peer(
        peer_id=peer_id,
        display_name="worker",
        path="/tmp/project",
        machine="test",
        backend=AgentType.CLAUDE_CODE,
        circle="dev",
        status=PeerStatus.ONLINE,
        last_seen=datetime.now(timezone.utc),
    )


def test_contradiction_tracker_dedups_until_cleared():
    tracker = PeerContradictionTracker()
    events: list[tuple[str, dict]] = []
    peer = _peer()

    def add_event(event_type: str, payload: dict) -> None:
        events.append((event_type, payload))

    tracker.emit(peer, "online_but_no_ws", "error", "missing ws", add_event)
    tracker.emit(peer, "online_but_no_ws", "error", "still missing", add_event)
    assert len(events) == 1

    tracker.clear(peer.peer_id, "online_but_no_ws")
    tracker.emit(peer, "online_but_no_ws", "error", "missing again", add_event)
    assert len(events) == 2


def test_contradiction_tracker_retries_after_event_sink_failure():
    tracker = PeerContradictionTracker()
    events: list[tuple[str, dict]] = []
    peer = _peer()

    def failing_add_event(event_type: str, payload: dict) -> None:
        raise RuntimeError("event store unavailable")

    def add_event(event_type: str, payload: dict) -> None:
        events.append((event_type, payload))

    tracker.emit(peer, "online_but_no_ws", "error", "missing ws", failing_add_event)
    tracker.emit(peer, "online_but_no_ws", "error", "missing ws", add_event)

    assert len(events) == 1


def test_contradiction_tracker_clear_all_is_peer_scoped():
    tracker = PeerContradictionTracker()
    events: list[tuple[str, dict]] = []
    peer1 = _peer("p1")
    peer2 = _peer("p2")

    def add_event(event_type: str, payload: dict) -> None:
        events.append((event_type, payload))

    tracker.emit(peer1, "a", "error", "one", add_event)
    tracker.emit(peer1, "b", "error", "two", add_event)
    tracker.emit(peer2, "a", "error", "three", add_event)

    tracker.clear_all(peer1.peer_id)
    tracker.emit(peer1, "a", "error", "again", add_event)
    tracker.emit(peer2, "a", "error", "again", add_event)

    assert [event[1]["peer_id"] for event in events].count("p1") == 3
    assert [event[1]["peer_id"] for event in events].count("p2") == 1
