"""Tmux lifecycle hook registration.

Installs/uninstalls global tmux hooks that POST to the daemon's
/hooks/lifecycle/* endpoints on pane/session/window events.

This is the ONLY module that knows about `tmux set-hook`.
"""

from __future__ import annotations

import logging
import subprocess

from repowire.hooks._tmux import is_tmux_available

logger = logging.getLogger(__name__)

# Re-export for callers that import from this module.
__all__ = ["is_tmux_available", "install_hooks", "uninstall_hooks"]

# Tmux hook → curl command templates.
# Format variables (#{...}) are expanded by tmux at fire time.
# Named array hooks ([repowire] suffix) avoid clobbering user hooks.
_HOOKS: dict[str, str] = {
    "pane-died": (
        'curl -sf -X POST http://{host}:{port}/hooks/lifecycle/pane-died'
        ' -H "Content-Type: application/json"'
        ' -d \'{{"pane_id":"#{{pane_id}}"}}\''
    ),
    "session-closed": (
        'curl -sf -X POST http://{host}:{port}/hooks/lifecycle/session-closed'
        ' -H "Content-Type: application/json"'
        ' -d \'{{"session_name":"#{{session_name}}"}}\''
    ),
    "session-renamed": (
        'curl -sf -X POST http://{host}:{port}/hooks/lifecycle/session-renamed'
        ' -H "Content-Type: application/json"'
        ' -d \'{{"old_name":"#{{hook_session_name}}","new_name":"#{{session_name}}"}}\''
    ),
    "window-renamed": (
        'curl -sf -X POST http://{host}:{port}/hooks/lifecycle/window-renamed'
        ' -H "Content-Type: application/json"'
        ' -d \'{{"session_name":"#{{session_name}}"'
        ',"old_name":"#{{hook_window_name}}","new_name":"#{{window_name}}"}}\''
    ),
    "client-detached": (
        'curl -sf -X POST http://{host}:{port}/hooks/lifecycle/client-detached'
        ' -H "Content-Type: application/json"'
        ' -d \'{{"session_name":"#{{session_name}}"}}\''
    ),
}


def install_hooks(host: str = "127.0.0.1", port: int = 8377) -> list[str]:
    """Install tmux lifecycle hooks. Idempotent.

    Returns list of hook names successfully installed.
    """
    installed: list[str] = []
    for hook_name, cmd_template in _HOOKS.items():
        cmd = cmd_template.format(host=host, port=port)
        result = subprocess.run(
            ["tmux", "set-hook", "-g", f"{hook_name}[repowire]", "run-shell", cmd],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            installed.append(hook_name)
        else:
            logger.warning("Failed to install tmux hook %s: %s", hook_name, result.stderr.strip())
    return installed


def uninstall_hooks() -> list[str]:
    """Remove all repowire tmux hooks.

    Returns list of hook names successfully removed.
    """
    removed: list[str] = []
    for hook_name in _HOOKS:
        result = subprocess.run(
            ["tmux", "set-hook", "-gu", f"{hook_name}[repowire]"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            removed.append(hook_name)
    return removed
