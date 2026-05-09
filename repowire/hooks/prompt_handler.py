#!/usr/bin/env python3
"""Handle UserPromptSubmit / BeforeAgent hook - marks peer as BUSY."""

from __future__ import annotations

import json
import sys

from repowire.hooks._tmux import get_pane_id
from repowire.hooks.adapters import hook_output, normalize
from repowire.hooks.utils import consume_reminder_buffer, update_status


def main(backend: str = "claude-code") -> int:
    """Main entry point for prompt hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"repowire prompt: invalid JSON input: {e}", file=sys.stderr)
        return 0

    payload = normalize(input_data, backend)

    if payload.event != "UserPromptSubmit":
        return 0

    pane_id = get_pane_id()
    if pane_id:
        if not update_status(pane_id, "busy", use_pane_id=True):
            print(
                f"repowire prompt: failed to update status for pane {pane_id}",
                file=sys.stderr,
            )

    # Ask-ack reminder injection: if the previous Stop hook flagged any
    # un-acked asks past the grace window, surface them as additionalContext
    # so the agent sees them at the start of this turn. Buffer is consumed
    # (deleted) on read so the same reminder doesn't repeat.
    reminder = consume_reminder_buffer(pane_id) if pane_id else None
    if reminder and backend == "claude-code":
        # Claude Code reads hookSpecificOutput.additionalContext on
        # UserPromptSubmit and prepends it to the agent's context for the turn.
        print(json.dumps({
            "hookSpecificOutput": {
                "hookEventName": "UserPromptSubmit",
                "additionalContext": reminder,
            }
        }))
        return 0

    hook_output(backend)
    return 0


if __name__ == "__main__":
    sys.exit(main())
