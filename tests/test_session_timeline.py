"""Tests for session timeline assembly."""

from __future__ import annotations

from repowire.session.history import Turn
from repowire.session.timeline import build_session_timeline


def _turn(text: str, *, turn_id: str, ts: str = "2026-01-01T00:00:00Z") -> Turn:
    return Turn(
        role="assistant",
        text=text,
        timestamp=ts,
        session_id="s1",
        turn_id=turn_id,
        tool_calls=[],
    )


def test_realtime_final_replaces_matching_history_turn() -> None:
    items = build_session_timeline(
        history_turns=[_turn("history", turn_id="t1")],
        events=[
            {
                "id": "e1",
                "type": "chat_turn",
                "timestamp": "2026-01-01T00:00:01Z",
                "peer_id": "peer-1",
                "peer": "worker",
                "role": "assistant",
                "text": "live",
                "session_id": "s1",
                "turn_id": "t1",
            }
        ],
        peer_id="peer-1",
        peer_names={"worker"},
    )

    assert [(item.source, item.text) for item in items] == [("realtime", "live")]
    assert items[0].session_id == "s1"
    assert items[0].turn_id == "t1"


def test_deltas_coalesce_until_final_turn_exists() -> None:
    events = [
        {
            "id": "d2",
            "type": "chat_turn_delta",
            "timestamp": "2026-01-01T00:00:02Z",
            "peer_id": "peer-1",
            "peer": "worker",
            "session_id": "s1",
            "turn_id": "t2",
            "chunk_index": 1,
            "kind": "text",
            "text": "second",
        },
        {
            "id": "d1",
            "type": "chat_turn_delta",
            "timestamp": "2026-01-01T00:00:01Z",
            "peer_id": "peer-1",
            "peer": "worker",
            "session_id": "s1",
            "turn_id": "t2",
            "chunk_index": 0,
            "kind": "text",
            "text": "first",
        },
    ]
    items = build_session_timeline(
        history_turns=[],
        events=events,
        peer_id="peer-1",
        peer_names={"worker"},
    )
    assert len(items) == 1
    assert items[0].kind == "delta_group"
    assert items[0].text == "first\n\nsecond"
    assert items[0].event_ids == ["d1", "d2"]

    finalized = build_session_timeline(
        history_turns=[_turn("final", turn_id="t2", ts="2026-01-01T00:00:03Z")],
        events=events,
        peer_id="peer-1",
        peer_names={"worker"},
    )
    assert [(item.kind, item.text) for item in finalized] == [("turn", "final")]


def test_session_filter_applies_to_history_and_events() -> None:
    history = [
        _turn("keep history", turn_id="t1"),
        Turn(
            role="assistant",
            text="drop history",
            timestamp="2026-01-01T00:00:00Z",
            session_id="s2",
            turn_id="t2",
            tool_calls=[],
        ),
    ]
    events = [
        {
            "id": "e1",
            "type": "chat_turn",
            "timestamp": "2026-01-01T00:00:01Z",
            "peer_id": "peer-1",
            "peer": "worker",
            "role": "assistant",
            "text": "drop event",
            "session_id": "s2",
            "turn_id": "t3",
        }
    ]

    items = build_session_timeline(
        history_turns=history,
        events=events,
        peer_id="peer-1",
        peer_names={"worker"},
        session_id="s1",
    )

    assert [item.text for item in items] == ["keep history"]
