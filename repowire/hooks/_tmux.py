"""Shared tmux utilities for hooks."""
from __future__ import annotations

import os
import subprocess


def get_tmux_target() -> str | None:
    """Get current tmux session:window from environment.

    Returns the tmux target in 'session:window' format, or None if not in tmux.
    """
    pane = os.environ.get("TMUX_PANE")
    if not pane:
        return None

    try:
        result = subprocess.run(
            ["tmux", "display-message", "-t", pane, "-p", "#{session_name}:#{window_name}"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except Exception:
        pass

    return None
