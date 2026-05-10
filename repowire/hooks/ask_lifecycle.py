"""Stop-hook reminder logic for the ask/ack lifecycle.

Pickup is reported transport-side at delivery time (see ws-hook /
opencode plugin / channel server) — never from this module. The Stop
hook's only role here is reminder fetch + injection: query the daemon
for picked-up-but-not-acked asks past the grace window, filter out
any cids the agent already acked/replied to in the just-completed turn,
and persist the rendered reminder block for the next prompt to inject.
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any
from urllib.parse import quote

from repowire.hooks.utils import daemon_get, daemon_post
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
    """Fetch due reminders, filter out ones acked/replied this turn.

    The daemon-side filter handles picked_up + reminded + grace-window. The
    client-side filter here drops cids the agent already acked/replied to
    via tool calls in the just-completed turn (the only signal visible from
    the transcript).
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
            # Filter only — do NOT auto-close. The agent's ack() tool call
            # is in flight to the daemon at the same time as this hook;
            # racing /ack here can land first with no message body and
            # close the ask before the real ack-with-msg arrives, dropping
            # the reply. The MCP tool call is the source of truth for
            # closure; this hook just prevents same-turn reminders.
            continue
        pending.append(ask)

    for ask in pending:
        cid = ask.get("correlation_id", "")
        if cid:
            daemon_post(
                f"/asks/{cid}/mark_reminded",
                {"correlation_id": cid},
            )

    return pending


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
