#!/usr/bin/env python3
"""Stop hook handler - captures responses and sends to daemon via HTTP."""
from __future__ import annotations

import json
import os
import sys
import urllib.request
import urllib.error
from pathlib import Path

from repowire.hooks._tmux import get_tmux_target
from repowire.session.transcript import extract_last_assistant_response

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")
PENDING_DIR = Path.home() / ".repowire" / "pending"


def tmux_to_filename(tmux_session: str) -> str:
    """Convert tmux session:window to safe filename."""
    return tmux_session.replace(":", "_").replace("/", "_")


def send_to_daemon(correlation_id: str, response: str) -> bool:
    """Send a response to the daemon via HTTP."""
    try:
        data = json.dumps({
            "correlation_id": correlation_id,
            "response": response,
        }).encode("utf-8")

        req = urllib.request.Request(
            f"{DAEMON_URL}/hook/response",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        with urllib.request.urlopen(req, timeout=5.0) as resp:
            return resp.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError, OSError):
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

    transcript_path_str = input_data.get("transcript_path")
    if not transcript_path_str:
        return 0

    # Get tmux target from environment - this is stable across session restarts
    tmux_target = get_tmux_target()
    if not tmux_target:
        return 0

    # Check if there's a pending query for this tmux pane
    pending_filename = tmux_to_filename(tmux_target)
    pending_file = PENDING_DIR / f"{pending_filename}.json"
    if not pending_file.exists():
        return 0

    try:
        with open(pending_file, "r") as f:
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
        send_to_daemon(correlation_id, response)

    # Clean up pending file
    pending_file.unlink(missing_ok=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
