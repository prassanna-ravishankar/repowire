#!/usr/bin/env python3
"""Handle UserPromptSubmit hook - marks peer as BUSY."""
from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def update_status(peer_name: str, status: str) -> bool:
    """Update peer status via daemon HTTP API."""
    try:
        data = json.dumps({
            "peer_name": peer_name,
            "status": status,
        }).encode("utf-8")

        req = urllib.request.Request(
            f"{DAEMON_URL}/session/update",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        with urllib.request.urlopen(req, timeout=2.0) as resp:
            return resp.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError, OSError):
        return False


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
