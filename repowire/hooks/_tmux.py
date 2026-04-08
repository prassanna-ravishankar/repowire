"""Shared tmux utilities for hooks."""

from __future__ import annotations

import os
import shutil
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


def is_tmux_available() -> bool:
    """Check if tmux is installed and a server is reachable."""
    if not shutil.which("tmux"):
        return False
    try:
        result = subprocess.run(
            ["tmux", "display-message", "-p", ""],
            capture_output=True,
            timeout=3,
        )
        return result.returncode == 0
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        return False


def get_pane_id() -> str | None:
    """Get the current tmux pane ID.

    Prefers TMUX_PANE env var, falls back to querying tmux directly.
    The fallback handles agents (Codex, Gemini) whose hook subprocesses
    may not inherit TMUX_PANE.
    """
    pane_id = os.environ.get("TMUX_PANE")
    if pane_id:
        return pane_id

    # Fallback: query tmux directly. Only attempt if TMUX env var is set
    # (proves we're inside a tmux session). Without this guard, we'd get
    # the most-recently-active pane from a different session - wrong peer.
    if not os.environ.get("TMUX"):
        return None

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-p", "#{pane_id}"],
            capture_output=True,
            text=True,
            timeout=3,
        )
        if result.returncode == 0:
            pane_id = result.stdout.strip()
            if pane_id:
                return pane_id
    except (subprocess.SubprocessError, FileNotFoundError, OSError):
        pass

    return None


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
