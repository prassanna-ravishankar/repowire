"""Shared utilities for hook handlers."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DAEMON_URL = os.environ.get("REPOWIRE_DAEMON_URL", "http://127.0.0.1:8377")
HOOKS_CACHE_DIR = Path.home() / ".cache" / "repowire" / "hooks"


def get_pane_file(pane_id: str | None) -> str:
    """Normalize pane_id for use in cache filenames (strips % and path separators)."""
    sanitized = (pane_id or "unknown").replace("%", "").replace("/", "").replace("\\", "")
    return sanitized or "unknown"


def get_uname_path(pane_id: str) -> Path:
    """Get the .uname cache file path for a given pane_id."""
    return HOOKS_CACHE_DIR / f"{get_pane_file(pane_id)}.uname"


def get_display_name(pane_id: str | None = None) -> str:
    """Get display name from .uname file, fallback to cwd folder name."""
    pid = pane_id or os.environ.get("TMUX_PANE")
    if pid:
        try:
            name = get_uname_path(pid).read_text().strip()
            if name:
                return name
        except OSError:
            pass
    return Path.cwd().name


def get_session_id_from_pane(pane_id: str) -> str | None:
    """Read session_id from the .sid file for a given pane_id.

    Returns the session_id (e.g., 'repow-dev-a1b2c3d4') or None if not available.
    """
    sid_file = HOOKS_CACHE_DIR / f"{get_pane_file(pane_id)}.sid"
    try:
        return sid_file.read_text().strip() or None
    except OSError:
        return None


def get_session_id() -> str | None:
    """Read session_id from the .sid file written by the WebSocket hook.

    Returns the session_id (e.g., 'repow-dev-a1b2c3d4') or None if not available.
    """
    pane_id = os.environ.get("TMUX_PANE")
    if not pane_id:
        return None
    return get_session_id_from_pane(pane_id)


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
