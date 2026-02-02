#!/usr/bin/env python3
"""Stop hook handler - captures responses and sends to daemon via HTTP.

This handler is invoked by Claude Code's Stop hook when processing completes.
It extracts the assistant's response from the transcript and sends it to the
daemon to resolve any pending query.

The pending file lookup uses the tmux pane ID (peer_id for claudemux) which
is available via the TMUX_PANE environment variable.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import DAEMON_URL, update_status
from repowire.session.transcript import extract_last_assistant_response

PENDING_DIR = Path.home() / ".repowire" / "pending"


def peer_id_to_filename(peer_id: str) -> str:
    """Convert peer_id to safe filename.

    For claudemux, peer_id is the tmux pane ID (e.g., "%42").
    The % is replaced with "pane_" for filesystem safety.
    """
    if peer_id.startswith("%"):
        return f"pane_{peer_id[1:]}"
    # Legacy format or fallback
    return peer_id.replace(":", "_").replace("/", "_").replace("%", "pane_")


def send_to_daemon(correlation_id: str, response: str) -> bool:
    """Send a response to the daemon via HTTP."""
    try:
        data = json.dumps(
            {
                "correlation_id": correlation_id,
                "response": response,
            }
        ).encode("utf-8")

        req = urllib.request.Request(
            f"{DAEMON_URL}/hook/response",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        with urllib.request.urlopen(req, timeout=5.0) as resp:
            return resp.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        print(f"repowire: daemon request failed: {e}", file=sys.stderr)
        return False


def main() -> int:
    """Main entry point for stop hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError:
        return 0

    # Don't process if already in a hook chain
    if input_data.get("stop_hook_active", False):
        return 0

    # Always mark peer as online when Claude finishes processing
    cwd = input_data.get("cwd", os.getcwd())
    peer_name = Path(cwd).name
    update_status(peer_name, "online")

    transcript_path_str = input_data.get("transcript_path")
    if not transcript_path_str:
        return 0

    # Get pane_id from environment - this is the peer_id for claudemux
    pane_id = get_pane_id()
    if not pane_id:
        return 0

    # Check if there's a pending query for this tmux pane
    pending_filename = peer_id_to_filename(pane_id)
    pending_file = PENDING_DIR / f"{pending_filename}.json"
    if not pending_file.exists():
        return 0

    try:
        with open(pending_file) as f:
            pending = json.load(f)
    except (json.JSONDecodeError, OSError):
        return 0

    correlation_id = pending.get("correlation_id")
    if not correlation_id:
        pending_file.unlink(missing_ok=True)
        return 0

    # Extract the response from transcript
    transcript_path = Path(transcript_path_str).expanduser()
    response = extract_last_assistant_response(transcript_path)

    if response:
        success = send_to_daemon(correlation_id, response)
        if not success:
            print(f"repowire: failed to deliver response for {correlation_id}", file=sys.stderr)

    # Clean up pending file
    pending_file.unlink(missing_ok=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
