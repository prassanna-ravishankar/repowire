#!/usr/bin/env python3
"""Handle Notification hook - marks peer as ONLINE on idle_prompt.

When Claude becomes idle (waiting for input for 60+ seconds), this hook
fires and resets the peer status to ONLINE. This handles cases where the
Stop hook doesn't fire (e.g., user interrupts with Escape).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

from repowire.hooks.utils import get_session_id, update_status


def main() -> int:
    """Main entry point for Notification hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire notification: invalid JSON input: {e}", file=sys.stderr)
        return 0

    if input_data.get("hook_event_name") != "Notification":
        return 0

    notification_type = input_data.get("notification_type")
    if notification_type != "idle_prompt":
        return 0

    # Claude is idle - mark peer as online
    cwd = input_data.get("cwd")
    if not cwd:
        return 0

    peer_identifier = get_session_id() or Path(cwd).name

    if not update_status(peer_identifier, "online"):
        print(
            f"repowire notification: failed to update status for {peer_identifier}",
            file=sys.stderr,
        )

    return 0


if __name__ == "__main__":
    sys.exit(main())
