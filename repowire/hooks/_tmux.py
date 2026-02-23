"""Shared tmux utilities for hooks."""

from __future__ import annotations

import os
import subprocess
from typing import TypedDict


class TmuxInfo(TypedDict):
    """Tmux environment information.

    The pane_id is the raw tmux pane ID (e.g., "%42"). It is used as a
    filename stem for .sid, .pid, correlation, and response cache files.
    The canonical peer_id is assigned by SessionMapper at WebSocket connect.
    """

    pane_id: str | None  # tmux pane ID, used as filename stem for hook files
    session_name: str | None
    window_name: str | None


def get_pane_id() -> str | None:
    """Get the current tmux pane ID from environment.

    Returns the tmux pane ID (e.g., "%42") or None if not in tmux.
    Used as a filename stem for hook files (.sid, .pid, correlation, response cache).
    """
    return os.environ.get("TMUX_PANE")


def get_tmux_info() -> TmuxInfo:
    """Get full tmux environment info.

    Returns a dict with pane_id, session_name, and window_name.
    All values will be None if not running in tmux.
    """
    pane_id = get_pane_id()
    if not pane_id:
        return {"pane_id": None, "session_name": None, "window_name": None}

    session_name = None
    window_name = None

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane_id, "-p", "#{session_name}:#{window_name}"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            parts = result.stdout.strip().split(":", 1)
            if len(parts) == 2:
                session_name, window_name = parts
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        pass

    return {"pane_id": pane_id, "session_name": session_name, "window_name": window_name}
