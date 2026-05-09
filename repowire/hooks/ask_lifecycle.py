"""Stop-hook helpers for the ask/ack lifecycle.

Two responsibilities:

  1. Pickup notification: pop any corr_ids the ws-hook pushed onto the per-pane
     FIFO and tell the daemon "this turn is when we picked them up." The daemon
     uses the turn sequence to enforce the one-turn grace before reminding.

  2. Reminder injection: detect acks/replies in the just-completed turn,
     query the daemon for picked-up-but-not-acked asks past the grace
     window, and emit additionalContext (or fall back to printing to stderr
     for backends that don't honor it) to nudge the agent.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path
from typing import Any

from repowire.hooks.utils import (
    daemon_get,
    daemon_post,
    pending_cid_path,
)
from repowire.session.transcript import extract_last_turn_raw_tool_calls

logger = logging.getLogger(__name__)


_ACK_TOOL_NAMES = ("ack", "mcp__repowire__ack")
_ASK_TOOL_NAMES = ("ask", "mcp__repowire__ask", "ask_peer", "mcp__repowire__ask_peer")


def _pop_all_pending_cids(pane_id: str) -> list[str]:
    """Drain the per-pane FIFO. Used at turn boundary for pickup recording."""
    import fcntl

    path = pending_cid_path(pane_id)
    lock_path = path.with_suffix(path.suffix + ".lock")
    try:
        with open(lock_path, "w") as lock_file:
            fcntl.flock(lock_file, fcntl.LOCK_EX)
            try:
                if not path.exists():
                    return []
                pending = json.loads(path.read_text())
                if not pending:
                    return []
                path.write_text("[]")
                return pending if isinstance(pending, list) else []
            finally:
                fcntl.flock(lock_file, fcntl.LOCK_UN)
    except (json.JSONDecodeError, OSError):
        return []


def _scan_acks_and_replies(transcript_path: Path | None) -> tuple[set[str], set[str]]:
    """Return (acked_cids, replied_to_cids) found in the last turn.

    acked_cids: corr_ids the agent acked (bare or with msg) via the `ack` tool.
    replied_to_cids: corr_ids the agent referenced as reply_to in a new ask
                     (which closes the prior ask).
    """
    acked: set[str] = set()
    replied_to: set[str] = set()
    if not transcript_path:
        return acked, replied_to

    raw_calls = extract_last_turn_raw_tool_calls(transcript_path)
    for call in raw_calls:
        name = call.get("name", "")
        # Tool names may be namespaced (mcp__repowire__ack) or bare (ack)
        bare = name.split("__")[-1] if "__" in name else name
        tool_input = call.get("input", {})
        if not isinstance(tool_input, dict):
            continue

        if bare in ("ack",) or name in _ACK_TOOL_NAMES:
            cid = tool_input.get("correlation_id") or tool_input.get("corr_id")
            if isinstance(cid, str) and cid:
                acked.add(cid)
        elif bare in ("ask", "ask_peer") or name in _ASK_TOOL_NAMES:
            reply_to = tool_input.get("reply_to")
            if isinstance(reply_to, str) and reply_to:
                replied_to.add(reply_to)

    return acked, replied_to


def record_pickups(pane_id: str, current_turn_seq: int) -> None:
    """Drain the FIFO and record each corr_id as picked-up at this turn.

    `current_turn_seq` is the SAME value returned by fetch_and_filter_pending
    in this Stop fire. Pickups tagged with seq=N produce N<N=false in the
    same-turn grace check, and N<N+1=true on the next Stop fire — exactly
    one turn of grace, regardless of call order.
    """
    pending = _pop_all_pending_cids(pane_id)
    for cid in pending:
        daemon_post(
            f"/asks/{cid}/picked_up",
            {
                "correlation_id": cid,
                "turn_seq": current_turn_seq,
                "pane_id": pane_id,
            },
        )


def fetch_and_filter_pending(
    pane_id: str,
    transcript_path: Path | None,
    self_peer_name: str,
) -> tuple[list[dict[str, Any]], int]:
    """Fetch due reminders, filter out ones acked/replied this turn.

    Returns (asks_to_inject, current_turn_seq). The caller passes
    current_turn_seq to record_pickups so newly-arrived asks get tagged
    with the turn that just ended (next Stop fire's grace window will
    flip them eligible).

    The daemon-side filter handles picked_up + reminded + grace-window. We
    apply the additional client-side filter for "this turn already
    handled it" because that's the only thing visible from the transcript.
    """
    result = daemon_get(f"/asks/pending?pane_id={pane_id}")
    if not result:
        return [], 0
    current_turn_seq = result.get("current_turn_seq", 0)
    asks = result.get("asks", [])
    if not asks:
        return [], current_turn_seq

    acked, replied_to = _scan_acks_and_replies(transcript_path)
    handled = acked | replied_to

    pending: list[dict[str, Any]] = []
    for ask in asks:
        cid = ask.get("correlation_id", "")
        if cid in handled:
            # Close on the daemon side so it doesn't keep showing up.
            # Closure requires a real ack() tool call — prose acks were
            # intentionally dropped because they trigger on accidental
            # mentions like "I'll do [ack #abc] later."
            if cid in acked:
                daemon_post(
                    "/ack",
                    {"correlation_id": cid, "from_peer": self_peer_name},
                )
            # reply_to closures already happened daemon-side at /ask time
            continue
        pending.append(ask)

    # Mark each as reminded so we don't nudge twice (once-only rule)
    for ask in pending:
        cid = ask.get("correlation_id", "")
        if cid:
            daemon_post(
                f"/asks/{cid}/mark_reminded",
                {"correlation_id": cid},
            )

    return pending, current_turn_seq


def format_reminder_block(asks: list[dict[str, Any]]) -> str:
    """Format a context-injection block listing un-acked asks."""
    if not asks:
        return ""
    lines = [
        "[repowire] You have un-acknowledged asks. Each needs ack(corr_id) "
        "to close (bare = seen-no-action), ack(corr_id, message) to reply, "
        "or ask(reply_to=corr_id, ...) to chain a follow-up.",
    ]
    for ask in asks:
        cid = ask.get("correlation_id", "")
        from_peer = ask.get("from_peer", "?")
        text = (ask.get("text") or "").strip()
        if len(text) > 200:
            text = text[:197] + "..."
        lines.append(f"  - #{cid} from @{from_peer}: {text}")
    return "\n".join(lines)
