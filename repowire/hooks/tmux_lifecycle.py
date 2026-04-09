"""Tmux lifecycle hook registration.

Installs/uninstalls tmux hooks that POST to the daemon's
/hooks/lifecycle/* endpoints on pane/session/window events.

This is the ONLY module that knows about `tmux set-hook`.
"""

from __future__ import annotations

import logging
import subprocess
from pathlib import Path

from repowire.hooks._tmux import is_tmux_available

logger = logging.getLogger(__name__)

# Re-export for callers that import from this module.
__all__ = ["is_tmux_available", "install_hooks", "uninstall_hooks"]

# Numeric array index — avoids clobbering user hooks at default index [0].
_HOOK_INDEX = 42

# Shell script for rename hooks (avoids tmux quoting hell with $()).
_RENAME_SCRIPT = Path(__file__).parent / "tmux_rename_hook.sh"

# Hook definitions: list of (tmux_hook_name, tmux_flag, shell_command).
#
# tmux_flag: "-g" for session-level hooks, "-gw" for window-level hooks.
# pane-exited (not pane-died, which requires remain-on-exit).
#
# Rename hooks call an external script because tmux's command parser
# can't handle $() subshells in run-shell arguments.
_HOOKS: list[tuple[str, str, str]] = [
    # -- pane exit --
    (
        "pane-exited",
        "-gw",
        "curl -sf -X POST http://{host}:{port}/hooks/lifecycle/pane-died"
        ' -H "Content-Type: application/json"'
        ' -d "{\\"pane_id\\":\\"#{pane_id}\\"}"',
    ),
    # -- session close --
    (
        "session-closed",
        "-g",
        "curl -sf -X POST"
        " http://{host}:{port}/hooks/lifecycle/session-closed"
        ' -H "Content-Type: application/json"'
        ' -d "{\\"session_name\\":\\"#{session_name}\\"}"',
    ),
    # -- session rename (post-rename, via helper script) --
    (
        "after-rename-session",
        "-g",
        "{script}"
        " http://{host}:{port}/hooks/lifecycle/session-renamed"
        " #{session_name} '' -s",
    ),
    # -- window rename (post-rename, via helper script) --
    (
        "after-rename-window",
        "-gw",
        "{script}"
        " http://{host}:{port}/hooks/lifecycle/window-renamed"
        " #{window_name} #{session_name} ''",
    ),
    # -- client detach --
    (
        "client-detached",
        "-g",
        "curl -sf -X POST"
        " http://{host}:{port}/hooks/lifecycle/client-detached"
        ' -H "Content-Type: application/json"'
        ' -d "{\\"session_name\\":\\"#{session_name}\\"}"',
    ),
]


def install_hooks(host: str = "127.0.0.1", port: int = 8377) -> list[str]:
    """Install tmux lifecycle hooks. Idempotent.

    Returns list of hook names successfully installed.
    """
    script = str(_RENAME_SCRIPT)
    installed: list[str] = []
    for hook_name, flag, cmd_template in _HOOKS:
        cmd = (
            cmd_template
            .replace("{host}", host)
            .replace("{port}", str(port))
            .replace("{script}", script)
        )
        tmux_cmd = f'run-shell "{cmd}"'
        result = subprocess.run(
            ["tmux", "set-hook", flag, f"{hook_name}[{_HOOK_INDEX}]", tmux_cmd],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            installed.append(hook_name)
        else:
            logger.warning(
                "Failed to install tmux hook %s: %s",
                hook_name, result.stderr.strip(),
            )
    return installed


def uninstall_hooks() -> list[str]:
    """Remove all repowire tmux hooks.

    Returns list of hook names successfully removed.
    """
    removed: list[str] = []
    for hook_name, flag, _ in _HOOKS:
        unsetter = flag + "u"  # -g → -gu, -gw → -gwu
        result = subprocess.run(
            ["tmux", "set-hook", unsetter, f"{hook_name}[{_HOOK_INDEX}]"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            removed.append(hook_name)
    return removed
