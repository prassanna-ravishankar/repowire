"""Session-scoped timeline assembly for dashboard/control surfaces."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Literal

from repowire.session.history import Turn

TimelineSource = Literal["history", "realtime"]
TimelineKind = Literal["turn", "delta_group"]


@dataclass
class TimelineItem:
    """One normalized session timeline item.

    ``session_id`` and ``turn_id`` are the stable reconciliation identity.
    ``peer_id`` is the live transport owner when known, so later control
    commands can target a durable session first and resolve the current peer
    separately.
    """

    id: str
    kind: TimelineKind
    source: TimelineSource
    timestamp: str
    session_id: str
    turn_id: str
    role: str
    text: str
    tool_calls: list[dict[str, str]] = field(default_factory=list)
    peer_id: str | None = None
    peer: str | None = None
    event_ids: list[str] = field(default_factory=list)


def timeline_key(session_id: str | None, turn_id: str | None) -> str | None:
    """Return the stable session/turn reconciliation key."""
    if not turn_id:
        return None
    return f"{session_id or 'legacy'}:{turn_id}"


def _event_belongs_to_peer(
    event: dict[str, Any],
    *,
    peer_id: str | None,
    peer_names: set[str],
) -> bool:
    event_peer_id = event.get("peer_id")
    if event_peer_id and peer_id:
        return event_peer_id == peer_id
    event_peer = event.get("peer")
    return isinstance(event_peer, str) and event_peer in peer_names


def _history_item(turn: Turn) -> TimelineItem:
    return TimelineItem(
        id=f"history:{turn.session_id}:{turn.turn_id}",
        kind="turn",
        source="history",
        timestamp=turn.timestamp,
        session_id=turn.session_id,
        turn_id=turn.turn_id,
        role=turn.role,
        text=turn.text,
        tool_calls=turn.tool_calls,
    )


def _chat_turn_item(event: dict[str, Any]) -> TimelineItem | None:
    turn_id = event.get("turn_id")
    if not isinstance(turn_id, str) or not turn_id:
        return None
    session_id = event.get("session_id") or "legacy"
    return TimelineItem(
        id=str(event.get("id") or f"event:{session_id}:{turn_id}"),
        kind="turn",
        source="realtime",
        timestamp=str(event.get("timestamp") or ""),
        session_id=str(session_id),
        turn_id=turn_id,
        role=str(event.get("role") or ""),
        text=str(event.get("text") or ""),
        tool_calls=list(event.get("tool_calls") or []),
        peer_id=event.get("peer_id"),
        peer=event.get("peer"),
        event_ids=[str(event["id"])] if event.get("id") else [],
    )


def _coalesce_delta_groups(
    events: list[dict[str, Any]],
    *,
    hidden_keys: set[str],
) -> list[TimelineItem]:
    groups: dict[str, TimelineItem] = {}
    group_events: dict[str, list[dict[str, Any]]] = {}

    for event in events:
        if event.get("type") != "chat_turn_delta":
            continue
        turn_id = event.get("turn_id")
        if not isinstance(turn_id, str) or not turn_id:
            continue
        session_id = str(event.get("session_id") or "legacy")
        key = timeline_key(session_id, turn_id)
        if key and key in hidden_keys:
            continue
        group_key = key or str(event.get("id") or turn_id)
        group_events.setdefault(group_key, []).append(event)
        if group_key not in groups:
            groups[group_key] = TimelineItem(
                id=f"delta:{session_id}:{turn_id}",
                kind="delta_group",
                source="realtime",
                timestamp=str(event.get("timestamp") or ""),
                session_id=session_id,
                turn_id=turn_id,
                role="assistant",
                text="",
                peer_id=event.get("peer_id"),
                peer=event.get("peer"),
            )
        elif str(event.get("timestamp") or "") > groups[group_key].timestamp:
            groups[group_key].timestamp = str(event.get("timestamp") or "")

    out: list[TimelineItem] = []
    for group_key, item in groups.items():
        ordered = sorted(
            group_events[group_key],
            key=lambda ev: (
                ev.get("chunk_index")
                if isinstance(ev.get("chunk_index"), int)
                else 2**31 - 1,
                str(ev.get("timestamp") or ""),
            ),
        )
        text_parts: list[str] = []
        for event in ordered:
            if event.get("id"):
                item.event_ids.append(str(event["id"]))
            if event.get("kind") == "tool_use" and isinstance(event.get("tool_call"), dict):
                item.tool_calls.append(event["tool_call"])
            elif event.get("kind") in (None, "text"):
                text = str(event.get("text") or "")
                if text:
                    text_parts.append(text)
        item.text = "\n\n".join(text_parts)
        if item.text or item.tool_calls:
            out.append(item)
    return out


def build_session_timeline(
    *,
    history_turns: list[Turn],
    events: list[dict[str, Any]],
    peer_id: str | None,
    peer_names: set[str],
    session_id: str | None = None,
) -> list[TimelineItem]:
    """Merge persisted turns and realtime chat events into one timeline.

    Realtime final ``chat_turn`` items win over persisted turns with the same
    ``session_id``/``turn_id``. Streaming deltas are grouped by the same key
    and hidden once either a persisted or realtime final turn exists.
    """
    history_items = [
        _history_item(turn)
        for turn in history_turns
        if session_id is None or turn.session_id == session_id
    ]
    history_keys = {
        key
        for item in history_items
        if (key := timeline_key(item.session_id, item.turn_id)) is not None
    }

    peer_events = [
        event
        for event in events
        if event.get("type") in {"chat_turn", "chat_turn_delta"}
        and _event_belongs_to_peer(event, peer_id=peer_id, peer_names=peer_names)
        and (session_id is None or (event.get("session_id") or "legacy") == session_id)
    ]

    realtime_turns = [
        item for event in peer_events if event.get("type") == "chat_turn"
        if (item := _chat_turn_item(event)) is not None
    ]
    realtime_keys = {
        key
        for item in realtime_turns
        if (key := timeline_key(item.session_id, item.turn_id)) is not None
    }

    items: list[TimelineItem] = []
    items.extend(realtime_turns)
    items.extend(
        item
        for item in history_items
        if timeline_key(item.session_id, item.turn_id) not in realtime_keys
    )
    items.extend(_coalesce_delta_groups(peer_events, hidden_keys=history_keys | realtime_keys))
    items.sort(key=lambda item: (item.timestamp, item.session_id, item.turn_id, item.source))
    return items
