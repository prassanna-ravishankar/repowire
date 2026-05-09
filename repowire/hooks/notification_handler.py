#!/usr/bin/env python3
"""Handle Notification hook - marks peer as ONLINE on idle_prompt.

When Claude becomes idle (waiting for input for 60+ seconds), this hook
fires and resets the peer status to ONLINE. This handles cases where the
Stop hook doesn't fire (e.g., user interrupts with Escape).
"""

from __future__ import annotations

import json
import sys

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.utils import consume_reminder_buffer, update_status


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

        # Idle is the secondary injection path for ask-ack reminders. If the
        # agent has gone idle (no follow-up user prompt) but has un-acked
        # asks, surface the reminder via additionalContext so it lands the
        # next time the agent does anything. Notification hooks support the
        # same hookSpecificOutput shape as UserPromptSubmit on Claude Code.
        reminder = consume_reminder_buffer(pane_id)
        if reminder:
            print(json.dumps({
                "hookSpecificOutput": {
                    "hookEventName": "Notification",
                    "additionalContext": reminder,
                }
            }))
            return 0

    return 0


if __name__ == "__main__":
    sys.exit(main())
