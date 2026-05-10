"""Stop-hook reminder logic for the ask/ack lifecycle.

Each Stop hook polls the daemon for open asks targeting this peer and
renders them as a reminder block for the next prompt. Backstop only —
the original ask was already pasted into the terminal by ws-hook when
the WS frame arrived. The reminder catches missed asks and asks the
agent hasn't yet acked. Open asks reappear in every Stop poll until
acked — no once-only flag, no grace window.

Client-side filter drops cids the agent already acked/replied to via
tool calls in the just-completed turn (their MCP ack call is in flight
to the daemon concurrently with this hook, so the daemon may not yet
reflect closure).
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any
from urllib.parse import quote

from repowire.hooks.utils import daemon_get
from repowire.session.transcript import extract_last_turn_raw_tool_calls

logger = logging.getLogger(__name__)


_ACK_BARE_NAMES = frozenset({"ack"})
_ASK_BARE_NAMES = frozenset({"ask", "ask_peer"})


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

    for call in extract_last_turn_raw_tool_calls(transcript_path):
        name = call.get("name", "")
        bare = name.rpartition("__")[2] or name
        tool_input = call.get("input", {})
        if not isinstance(tool_input, dict):
            continue

        if bare in _ACK_BARE_NAMES:
            cid = tool_input.get("correlation_id") or tool_input.get("corr_id")
            if isinstance(cid, str) and cid:
                acked.add(cid)
        elif bare in _ASK_BARE_NAMES:
            reply_to = tool_input.get("reply_to")
            if isinstance(reply_to, str) and reply_to:
                replied_to.add(reply_to)

    return acked, replied_to


def fetch_and_filter_pending(
    pane_id: str,
    transcript_path: Path | None,
) -> list[dict[str, Any]]:
    """Fetch open asks for this pane, filter out ones acked/replied this turn.

    The daemon returns every open ask for this peer. The client-side filter
    here drops cids the agent already acked/replied to via tool calls in the
    just-completed turn — those acks are in flight concurrently and the
    daemon may not yet reflect closure.
    """
    result = daemon_get(f"/asks/pending?pane_id={quote(pane_id, safe='')}")
    if not result:
        return []
    asks = result.get("asks", [])
    if not asks:
        return []

    acked, replied_to = _scan_acks_and_replies(transcript_path)
    handled = acked | replied_to

    pending: list[dict[str, Any]] = []
    for ask in asks:
        cid = ask.get("correlation_id", "")
        if cid in handled:
            continue
        pending.append(ask)

    return pending


_BODY_SNIPPET_CHARS = 150


def format_reminder_block(asks: list[dict[str, Any]]) -> str:
    """Compact reminder: cid + asker + a snippet of body per ask.

    The original ask was injected into the terminal by ws-hook at delivery
    time, so the full body is usually still in the agent's transcript. The
    snippet is a fallback for cases where the body fell out of context
    (compaction, missed paste, restart) — enough to recall the ask without
    the wall-of-text problem of re-pasting the full body every Stop cycle.
    """
    if not asks:
        return ""
    lines = [
        f"[repowire] {len(asks)} open ask(s). Handle each: ack(corr_id) bare "
        "if no reply needed, ack(corr_id, message) to reply.",
    ]
    for a in asks:
        cid = a.get("correlation_id", "?")
        from_peer = a.get("from_peer", "?")
        body = (a.get("text") or "").strip().replace("\n", " ")
        if len(body) > _BODY_SNIPPET_CHARS:
            body = body[: _BODY_SNIPPET_CHARS - 1] + "…"
        head = f"  - #{cid} from @{from_peer}"
        lines.append(f"{head}: {body}" if body else head)
    return "\n".join(lines)
