"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def get_session_id() -> str | None:
    """Read session_id from the .sid file written by the WebSocket hook.

    Returns the session_id (e.g., 'repow-dev-a1b2c3d4') or None if not available.
    """
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        return None

    pane_file = pane_id.replace("%", "")
    sid_file = Path.home() / ".cache" / "repowire" / "hooks" / f"{pane_file}.sid"
    if sid_file.exists():
        try:
            return sid_file.read_text().strip() or None
        except OSError as e:
            print(f"repowire: failed to read session ID file {sid_file}: {e}", file=sys.stderr)
    return None


def update_status(peer_identifier: str, status: str) -> bool:
    """Update peer status via daemon HTTP API.

    Args:
        peer_identifier: session_id (preferred) or display_name
        status: New status (online, busy, offline)
    """
    try:
        data = json.dumps(
            {
                "peer_name": peer_identifier,
                "status": status,
            }
        ).encode("utf-8")

        req = urllib.request.Request(
            f"{DAEMON_URL}/session/update",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        with urllib.request.urlopen(req, timeout=2.0) as resp:
            return resp.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        print(f"repowire: status update failed for {peer_identifier}: {e}", file=sys.stderr)
        return False
