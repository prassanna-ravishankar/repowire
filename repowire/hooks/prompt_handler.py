#!/usr/bin/env python3
"""Handle UserPromptSubmit hook - marks peer as BUSY."""

from __future__ import annotations

import json
import sys

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import update_status


def main() -> int:
    """Main entry point for UserPromptSubmit hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire prompt: invalid JSON input: {e}", file=sys.stderr)
        return 0

    if input_data.get("hook_event_name") != "UserPromptSubmit":
        return 0

    pane_id = get_pane_id()
    if pane_id:
        if not update_status(pane_id, "busy", use_pane_id=True):
            print(
                f"repowire prompt: failed to update status for pane {pane_id}",
                file=sys.stderr,
            )

    return 0


if __name__ == "__main__":
    sys.exit(main())
