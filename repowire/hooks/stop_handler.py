#!/usr/bin/env python3
"""Stop hook handler - captures responses and delivers to daemon via HTTP."""

from __future__ import annotations

import json
import os
import sys
import urllib.request
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import DAEMON_URL, update_status
from repowire.session.transcript import extract_last_turn_pair


def _post_chat_turn(peer_name: str, role: str, text: str) -> None:
    """Post a chat turn to the daemon for dashboard display. Best-effort."""
    try:
        data = json.dumps({"peer": peer_name, "role": role, "text": text}).encode("utf-8")
        req = urllib.request.Request(
            f"{DAEMON_URL}/events/chat",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        urllib.request.urlopen(req, timeout=2.0).close()
    except Exception as e:
        print(f"repowire: failed to post chat turn: {e}", file=sys.stderr)


def main() -> int:
    """Main entry point for stop hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire stop: invalid JSON input: {e}", file=sys.stderr)
        return 0

    if input_data.get("stop_hook_active", False):
        return 0

    # Always mark peer as online when Claude finishes processing
    cwd = input_data.get("cwd", os.getcwd())
    peer_display = Path(cwd).name
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
    if not transcript_path_str:
        return 0

    # Extract and post last turn pair for dashboard
    transcript_path = Path(transcript_path_str).expanduser().resolve()
    user_text, assistant_text = extract_last_turn_pair(transcript_path)
    if user_text:
        _post_chat_turn(peer_display, "user", user_text)
    if assistant_text:
        _post_chat_turn(peer_display, "assistant", assistant_text)

    # Deliver response to daemon for query resolution
    if pane_id and assistant_text:
        try:
            data = json.dumps({"pane_id": pane_id, "text": assistant_text}).encode("utf-8")
            req = urllib.request.Request(
                f"{DAEMON_URL}/response",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            urllib.request.urlopen(req, timeout=2.0).close()
        except Exception as e:
            print(f"repowire stop: failed to deliver response: {e}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
