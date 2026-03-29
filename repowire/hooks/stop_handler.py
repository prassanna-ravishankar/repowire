#!/usr/bin/env python3
"""Stop hook handler - captures responses and delivers to daemon via HTTP."""

from __future__ import annotations

import fcntl
import json
import os
import sys
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import daemon_post, derive_display_name, pending_cid_path, update_status
from repowire.session.transcript import extract_last_turn_pair, extract_last_turn_tool_calls


def _pop_pending_cid(pane_id: str) -> str | None:
    """Pop the oldest pending correlation_id for a pane, if any.

    Uses flock to prevent race with websocket_hook's _push_pending_cid.
    """
    path = pending_cid_path(pane_id)
    lock_path = path.with_suffix(path.suffix + ".lock")
    try:
        with open(lock_path, "w") as lock_file:
            fcntl.flock(lock_file, fcntl.LOCK_EX)
            try:
                if not path.exists():
                    return None
                pending = json.loads(path.read_text())
                if not pending:
                    return None
                cid = pending.pop(0)
                path.write_text(json.dumps(pending))
                return cid
            finally:
                fcntl.flock(lock_file, fcntl.LOCK_UN)
    except (json.JSONDecodeError, OSError, IndexError):
        return None


def _post_chat_turn(
    peer_name: str, role: str, text: str, tool_calls: list[dict[str, str]] | None = None,
) -> None:
    """Post a chat turn to the daemon for dashboard display. Best-effort."""
    payload: dict = {"peer": peer_name, "role": role, "text": text}
    if tool_calls:
        payload["tool_calls"] = tool_calls
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

    # Always mark peer as online when agent finishes processing
    cwd = input_data.get("cwd", os.getcwd())
    session_id = input_data.get("session_id", "")
    peer_display = derive_display_name(session_id, cwd)
    pane_id = get_pane_id()
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

    transcript_path_str = input_data.get("transcript_path")
    user_text = None
    assistant_text = input_data.get("final_response")
    tool_calls = []

    if transcript_path_str:
        # Extract and post last turn pair for dashboard
        transcript_path = Path(transcript_path_str).expanduser().resolve()
        user_text, transcript_assistant_text = extract_last_turn_pair(transcript_path)
        if transcript_assistant_text:
            assistant_text = transcript_assistant_text
        tool_calls = extract_last_turn_tool_calls(transcript_path) if assistant_text else []

    # Strip whitespace-only texts to prevent empty chat bubbles
    if user_text and not user_text.strip():
        user_text = None
    if assistant_text and not assistant_text.strip():
        assistant_text = None

    if user_text:
        _post_chat_turn(peer_display, "user", user_text)
    if assistant_text:
        _post_chat_turn(peer_display, "assistant", assistant_text, tool_calls or None)

    # Deliver response to daemon for query resolution
    if pane_id and assistant_text:
        payload: dict = {"pane_id": pane_id, "text": assistant_text}
        cid = _pop_pending_cid(pane_id)
        if cid:
            payload["correlation_id"] = cid
        daemon_post("/response", payload)

    # For Gemini, always print the decision (Claude is more lenient)
    print(json.dumps({"decision": "allow"}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
