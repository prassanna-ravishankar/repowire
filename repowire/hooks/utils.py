"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")


def get_pane_file(pane_id: str | None) -> str:
    """Normalize pane_id for use in cache filenames (strips % and path separators)."""
    sanitized = (pane_id or "unknown").replace("%", "").replace("/", "").replace("\\", "")
    return sanitized or "unknown"


def get_display_name() -> str:
    """Get display name from env var or cwd folder name."""
    name = os.environ.get("REPOWIRE_DISPLAY_NAME")
    if name:
        return name
    return Path.cwd().name


def update_status(peer_identifier: str, status_value: str, *, use_pane_id: bool = False) -> bool:
    """Update peer status via daemon HTTP API.

    Args:
        peer_identifier: session_id, display_name, or pane_id
        status_value: New status (online, busy, offline)
        use_pane_id: If True, send as pane_id instead of peer_name
    """
    try:
        if use_pane_id:
            payload = {"pane_id": peer_identifier, "status": status_value}
        else:
            payload = {"peer_name": peer_identifier, "status": status_value}
        data = json.dumps(payload).encode("utf-8")
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
