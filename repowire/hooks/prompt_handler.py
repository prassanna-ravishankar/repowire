#!/usr/bin/env python3
"""Handle UserPromptSubmit hook - marks peer as BUSY."""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

from repowire.hooks.utils import update_status


def main() -> int:
    """Main entry point for UserPromptSubmit hook."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError:
        return 0

    if input_data.get("hook_event_name") != "UserPromptSubmit":
        return 0

    cwd = input_data.get("cwd", os.getcwd())
    peer_name = Path(cwd).name

    update_status(peer_name, "busy")

    return 0


if __name__ == "__main__":
    sys.exit(main())
