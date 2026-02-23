#!/usr/bin/env python3
"""Stop hook handler - captures responses and writes to file for async hook.

This handler is invoked by Claude Code's Stop hook when processing completes.
It extracts the assistant's response from the transcript and writes it to a file
that the async WebSocket hook watches and forwards.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import get_session_id, update_status
from repowire.session.transcript import extract_last_assistant_response


def main() -> int:
    """Main entry point for stop hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire stop: invalid JSON input: {e}", file=sys.stderr)
        return 0

    # Don't process if already in a hook chain
    if input_data.get("stop_hook_active", False):
        return 0

    # Always mark peer as online when Claude finishes processing
    cwd = input_data.get("cwd", os.getcwd())
    peer_identifier = get_session_id() or Path(cwd).name
    if not update_status(peer_identifier, "online"):
        print(
            f"repowire stop: failed to update status for {peer_identifier}",
            file=sys.stderr,
        )

    transcript_path_str = input_data.get("transcript_path")
    if not transcript_path_str:
        return 0

    # Get pane_id from environment
    pane_id = get_pane_id()
    if not pane_id:
        print("repowire stop: TMUX_PANE not set", file=sys.stderr)
        return 0

    # Check if there's a correlation_id stored for this pane
    correlation_dir = Path.home() / ".cache" / "repowire" / "correlations"
    pane_file = pane_id.replace("%", "")
    corr_file = correlation_dir / pane_file

    if not corr_file.exists():
        return 0  # Not a query response

    try:
        correlation_id = corr_file.read_text().strip()
    except OSError as e:
        print(f"repowire stop: error reading correlation file: {e}", file=sys.stderr)
        return 0

    # Extract the response from transcript
    transcript_path = Path(transcript_path_str).expanduser()
    response = extract_last_assistant_response(transcript_path)

    if response:
        # Write response to file for async hook to forward
        response_dir = Path.home() / ".cache" / "repowire" / "responses"
        response_dir.mkdir(parents=True, exist_ok=True)

        response_file = response_dir / f"{pane_file}.json"
        tmp_file = response_dir / f"{pane_file}.json.tmp"
        try:
            payload = json.dumps({"correlation_id": correlation_id, "response": response})
            tmp_file.write_text(payload)
            os.replace(str(tmp_file), str(response_file))
        except OSError as e:
            print(
                f"repowire stop: failed to write response file for {correlation_id[:8]}: {e}",
                file=sys.stderr,
            )

    # Clean up correlation file
    corr_file.unlink(missing_ok=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
