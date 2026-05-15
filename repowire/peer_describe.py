"""Snapshot one peer's full state for `repowire peer describe`.

Pure functions: each takes an httpx.Client + daemon_url and returns plain
data. The CLI layer (cli.py) handles rendering and exit codes. Tests mock
the httpx transport — no live daemon required.

Resolution rules mirror the daemon's own ambiguity refusal:

  - Accept either display_name or peer_id.
  - If `circle=` is supplied, filter to that circle.
  - If 2+ matches survive and no circle is supplied, refuse with the same
    message format the daemon emits (issue #136).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

import httpx


@dataclass
class PendingAsk:
    """One open ask thread (either direction)."""

    correlation_id: str
    direction: str  # "inbound" | "outbound"
    from_peer: str
    to_peer: str
    text: str
    created_at: str


@dataclass
class ActivityEvent:
    """One recent communication event involving this peer."""

    event_type: str  # "notification" | "query" | "response" | "chat_turn" | ...
    timestamp: str  # ISO
    direction: str  # "inbound" | "outbound" | "self"
    counterparty: str  # display_name of the other side, or "" for chat_turn
    text: str


@dataclass
class PeerSnapshot:
    """Everything known about one peer."""

    peer: dict[str, Any]
    inbound_asks: list[PendingAsk] = field(default_factory=list)
    outbound_asks: list[PendingAsk] = field(default_factory=list)
    recent_activity: list[ActivityEvent] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Resolution
# ---------------------------------------------------------------------------


@dataclass
class ResolveNotFound:
    identifier: str
    known_names: list[str]


@dataclass
class ResolveAmbiguous:
    identifier: str
    circles: list[str]


def resolve_peer(
    peers: list[dict[str, Any]],
    identifier: str,
    circle: str | None = None,
) -> dict[str, Any] | ResolveNotFound | ResolveAmbiguous:
    """Resolve identifier to a single peer dict, or report failure.

    Tries peer_id first (always unique), then display_name. When
    display_name yields multiple matches:
      - filter by circle if supplied
      - if still ambiguous, return ResolveAmbiguous so the caller can show
        the same refusal format the daemon emits internally
    """
    by_id = [p for p in peers if p.get("peer_id") == identifier]
    if by_id:
        return by_id[0]

    by_name = [p for p in peers if p.get("display_name") == identifier]
    if not by_name:
        return ResolveNotFound(
            identifier=identifier,
            known_names=sorted({p.get("display_name", "") for p in peers if p.get("display_name")}),
        )

    if circle is not None:
        by_name = [p for p in by_name if p.get("circle") == circle]
        if not by_name:
            return ResolveNotFound(
                identifier=identifier,
                known_names=sorted(
                    {p.get("display_name", "") for p in peers if p.get("display_name")},
                ),
            )

    if len(by_name) == 1:
        return by_name[0]

    return ResolveAmbiguous(
        identifier=identifier,
        circles=sorted({p.get("circle", "global") for p in by_name}),
    )


def ambiguity_message(amb: ResolveAmbiguous) -> str:
    """Match the daemon's refusal format (peer_registry._lookup_peer_unlocked)."""
    return (
        f"Ambiguous peer name {amb.identifier!r}: matches in circles "
        f"{amb.circles}. Specify --circle= or pass a peer_id."
    )


# ---------------------------------------------------------------------------
# HTTP fetchers
# ---------------------------------------------------------------------------


def fetch_all_peers(client: httpx.Client, daemon_url: str) -> list[dict[str, Any]]:
    """GET /peers — return all peers (online + offline)."""
    resp = client.get(f"{daemon_url}/peers")
    resp.raise_for_status()
    return resp.json().get("peers", [])


def fetch_pending_asks(
    client: httpx.Client,
    daemon_url: str,
    peer_id: str,
    direction: str = "both",
) -> list[PendingAsk]:
    """GET /asks/pending?peer_id=...&direction=... — open asks for one peer.

    Returns [] if the daemon doesn't recognize the peer_id (e.g. just
    pruned) rather than raising — describe is a read-only inspection and
    a stale id shouldn't break the render.
    """
    resp = client.get(
        f"{daemon_url}/asks/pending",
        params={"peer_id": peer_id, "direction": direction},
    )
    if resp.status_code == 404:
        return []
    resp.raise_for_status()
    items = resp.json().get("asks", [])
    return [
        PendingAsk(
            correlation_id=a["correlation_id"],
            direction=a.get("direction", "inbound"),
            from_peer=a.get("from_peer", ""),
            to_peer=a.get("to_peer", ""),
            text=a.get("text", ""),
            created_at=a.get("created_at", ""),
        )
        for a in items
    ]


def fetch_recent_activity(
    client: httpx.Client,
    daemon_url: str,
    peer_id: str,
    peer_name: str,
    limit: int = 5,
) -> list[ActivityEvent]:
    """GET /events, filter to this peer, newest first, cap at limit."""
    resp = client.get(f"{daemon_url}/events")
    resp.raise_for_status()
    events = resp.json()
    if not isinstance(events, list):
        return []

    out: list[ActivityEvent] = []
    for ev in events:
        if not isinstance(ev, dict):
            continue
        from_id = ev.get("from_peer_id")
        to_id = ev.get("to_peer_id")
        from_name = ev.get("from") or ev.get("peer") or ""
        to_name = ev.get("to") or ""

        # Match by peer_id (precise) or display_name (events from older
        # versions or chat_turn events that only carry the name).
        is_from = from_id == peer_id or from_name == peer_name
        is_to = to_id == peer_id or to_name == peer_name
        if not (is_from or is_to):
            continue

        if is_from and is_to:
            direction = "self"
            counterparty = ""
        elif is_from:
            direction = "outbound"
            counterparty = to_name
        else:
            direction = "inbound"
            counterparty = from_name

        out.append(
            ActivityEvent(
                event_type=ev.get("type", "?"),
                timestamp=ev.get("timestamp", ""),
                direction=direction,
                counterparty=counterparty,
                text=str(ev.get("text", ""))[:120],
            ),
        )

    # Events are stored append-order (oldest first). Reverse for newest-first.
    out.reverse()
    return out[:limit]


def build_snapshot(
    client: httpx.Client,
    daemon_url: str,
    peer: dict[str, Any],
) -> PeerSnapshot:
    """Build the full snapshot for an already-resolved peer."""
    peer_id = peer["peer_id"]
    peer_name = peer.get("display_name") or peer.get("name") or peer_id

    inbound: list[PendingAsk] = []
    outbound: list[PendingAsk] = []
    try:
        all_asks = fetch_pending_asks(client, daemon_url, peer_id, direction="both")
        for a in all_asks:
            (inbound if a.direction == "inbound" else outbound).append(a)
    except httpx.HTTPError:
        # Older daemon without direction= support — fall back to inbound only.
        try:
            inbound = fetch_pending_asks(client, daemon_url, peer_id, direction="inbound")
        except httpx.HTTPError:
            pass

    activity: list[ActivityEvent] = []
    try:
        activity = fetch_recent_activity(client, daemon_url, peer_id, peer_name)
    except httpx.HTTPError:
        pass

    return PeerSnapshot(
        peer=peer,
        inbound_asks=inbound,
        outbound_asks=outbound,
        recent_activity=activity,
    )


# ---------------------------------------------------------------------------
# Time formatting helpers (pure, testable)
# ---------------------------------------------------------------------------


def humanize_last_seen(iso: str | None, now: datetime | None = None) -> str:
    """'2m ago (14:55 UTC)' style. Empty string if iso is None/empty."""
    if not iso:
        return "never"
    try:
        ts = datetime.fromisoformat(iso.replace("Z", "+00:00"))
    except ValueError:
        return iso
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    current = now or datetime.now(timezone.utc)
    delta = current - ts
    secs = int(delta.total_seconds())
    if secs < 0:
        rel = "in the future"
    elif secs < 60:
        rel = f"{secs}s ago"
    elif secs < 3600:
        rel = f"{secs // 60}m ago"
    elif secs < 86400:
        rel = f"{secs // 3600}h ago"
    else:
        rel = f"{secs // 86400}d ago"
    return f"{rel} ({ts.strftime('%H:%M %Z') or ts.strftime('%H:%M UTC')})"
