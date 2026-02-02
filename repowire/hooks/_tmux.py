"""Shared tmux utilities for hooks."""

from __future__ import annotations

import os
import subprocess
from typing import TypedDict


class TmuxInfo(TypedDict):
    """Tmux environment information.

    The pane_id here is the raw tmux pane ID (e.g., "%42") which is used
    as the peer_id for claudemux backend peers.
    """

    pane_id: str | None  # tmux pane ID, used as peer_id for claudemux
    session_name: str | None
    window_name: str | None


def get_pane_id() -> str | None:
    """Get the current tmux pane ID from environment.

    Returns the tmux pane ID (e.g., "%42") or None if not in tmux.
    For claudemux backend, this is used as the peer_id.
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
    except Exception:
        pass

    return {"pane_id": pane_id, "session_name": session_name, "window_name": window_name}


def get_tmux_target() -> str | None:
    """Get current tmux session:window from environment.

    Returns the tmux target in 'session:window' format, or None if not in tmux.

    Note: Kept for backward compatibility. Prefer get_tmux_info() for new code.
    """
    info = get_tmux_info()
    if info["session_name"] and info["window_name"]:
        return f"{info['session_name']}:{info['window_name']}"
    return None
