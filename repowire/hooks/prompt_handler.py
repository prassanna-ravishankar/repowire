#!/usr/bin/env python3
"""Handle UserPromptSubmit hook - marks peer as BUSY."""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

from repowire.hooks.utils import get_session_id, update_status


def main() -> int:
    """Main entry point for UserPromptSubmit hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire prompt: invalid JSON input: {e}", file=sys.stderr)
        return 0

    if input_data.get("hook_event_name") != "UserPromptSubmit":
        return 0

    cwd = input_data.get("cwd", os.getcwd())
    peer_identifier = get_session_id() or Path(cwd).name

    if not update_status(peer_identifier, "busy"):
        print(
            f"repowire prompt: failed to update status for {peer_identifier}",
            file=sys.stderr,
        )

    return 0


if __name__ == "__main__":
    sys.exit(main())
