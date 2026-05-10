#!/usr/bin/env python3
"""Stop / AfterAgent hook handler - captures responses and delivers to daemon."""

from __future__ import annotations

import json
import sys
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.adapters import hook_output, normalize
from repowire.hooks.ask_lifecycle import fetch_and_filter_pending, format_reminder_block
from repowire.hooks.utils import (
    daemon_post,
    get_display_name,
    pop_query_cid,
    update_status,
    write_reminder_buffer,
)
from repowire.session.transcript import extract_last_turn_pair, extract_last_turn_tool_calls


def _post_chat_turn(
    peer_name: str,
    role: str,
    text: str,
    tool_calls: list[dict[str, str]] | None = None,
    pane_id: str | None = None,
) -> None:
    """Post a chat turn to the daemon for dashboard display. Best-effort."""
    payload: dict = {"peer": peer_name, "role": role, "text": text}
    if tool_calls:
        payload["tool_calls"] = tool_calls
    if pane_id:
        payload["pane_id"] = pane_id
    daemon_post("/events/chat", payload)


def main(backend: str = "claude-code") -> int:
    """Main entry point for stop hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire stop: invalid JSON input: {e}", file=sys.stderr)
        return 0

    if input_data.get("stop_hook_active", False):
        return 0

    payload = normalize(input_data, backend)

    peer_display = get_display_name()
    pane_id = get_pane_id()

    # Get response text: adapter extracts from agent-specific fields,
    # fall back to transcript parsing for Claude Code
    assistant_text = payload.response_text
    user_text = None
    tool_calls: list = []

    if payload.transcript_path:
        transcript_path = Path(payload.transcript_path).expanduser().resolve()
        user_text, transcript_text = extract_last_turn_pair(transcript_path)
        if transcript_text:
            assistant_text = transcript_text
        tool_calls = extract_last_turn_tool_calls(transcript_path) if assistant_text else []

    # Strip whitespace-only texts to prevent empty chat bubbles
    if user_text and not user_text.strip():
        user_text = None
    if assistant_text and not assistant_text.strip():
        assistant_text = None

    if user_text:
        _post_chat_turn(peer_display, "user", user_text, pane_id=pane_id)
    if assistant_text:
        _post_chat_turn(
            peer_display, "assistant", assistant_text, tool_calls or None, pane_id=pane_id,
        )

    # Deliver response to daemon for legacy /query future resolution.
    # The query FIFO is single-purpose (only /query cids land here), so this
    # never collides with the ask-ack lifecycle.
    if pane_id and assistant_text:
        resp_payload: dict = {"pane_id": pane_id, "text": assistant_text}
        cid = pop_query_cid(pane_id)
        if cid:
            resp_payload["correlation_id"] = cid
        daemon_post("/response", resp_payload)

    # Ask-ack reminder fetch MUST happen before update_status. update_status
    # transitions the peer from BUSY to online, which drains the BUSY buffer
    # in the daemon and triggers fresh ask deliveries. Those deliveries POST
    # pickup at the daemon's current turn_seq. If we bumped turn_seq AFTER
    # the drain, those pickups would snapshot N and the same Stop's pending
    # poll would flag them (N<N+1) — same-turn reminder, exactly the bug
    # we're trying to avoid. By bumping first, drained-asks snapshot N+1
    # and don't become eligible until the NEXT Stop.
    if pane_id:
        transcript_path = (
            Path(payload.transcript_path).expanduser().resolve()
            if payload.transcript_path else None
        )
        due = fetch_and_filter_pending(pane_id, transcript_path)
        if due:
            write_reminder_buffer(pane_id, format_reminder_block(due))

    # Mark peer online — drains BUSY buffer if any. Now safe re: above.
    if pane_id:
        if not update_status(pane_id, "online", use_pane_id=True):
            print(
                f"repowire stop: failed to update status for pane {pane_id}",
                file=sys.stderr,
            )
    else:
        if not update_status(peer_display, "online"):
            print(
                f"repowire stop: failed to update status for {peer_display}",
                file=sys.stderr,
            )

    hook_output(backend)
    return 0


if __name__ == "__main__":
    sys.exit(main())
