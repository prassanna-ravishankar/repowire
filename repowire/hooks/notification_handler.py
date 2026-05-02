#!/usr/bin/env python3
"""Handle Notification hook - marks peer as ONLINE on idle_prompt.

When Claude becomes idle (waiting for input for 60+ seconds), this hook
fires and resets the peer status to ONLINE. This handles cases where the
Stop hook doesn't fire (e.g., user interrupts with Escape).
"""

from __future__ import annotations

import fcntl
import json
import sys

from repowire.hooks._tmux import get_pane_id, send_tmux_keys
from repowire.hooks.utils import (
    pending_notification_path,
    read_pane_runtime_metadata,
    update_status,
    write_pane_runtime_metadata,
)


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

    pane_id = get_pane_id()
    if pane_id:
        if not update_status(pane_id, "online", use_pane_id=True):
            print(
                f"repowire notification: failed to update status for pane {pane_id}",
                file=sys.stderr,
            )
        metadata = read_pane_runtime_metadata(pane_id)
        metadata["status"] = "online"
        write_pane_runtime_metadata(pane_id, metadata)
        _flush_pending_notifications(pane_id)

    return 0


def _pop_pending_notification(pane_id: str) -> str | None:
    """Pop the oldest pending notification for a pane, if any.

    Uses flock to prevent race with websocket_hook.
    """
    path = pending_notification_path(pane_id)
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
                message = pending.pop(0)
                path.write_text(json.dumps(pending))
                return message
            finally:
                fcntl.flock(lock_file, fcntl.LOCK_UN)
    except (json.JSONDecodeError, OSError, IndexError):
        return None


def _flush_pending_notifications(pane_id: str) -> None:
    """Deliver all pending notifications into the pane."""
    while True:
        message = _pop_pending_notification(pane_id)
        if not message:
            return
        if not send_tmux_keys(pane_id, message):
            return


if __name__ == "__main__":
    sys.exit(main())
