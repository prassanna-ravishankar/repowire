"""Diagnostics for spawned peers that fail to self-register."""

from __future__ import annotations

import asyncio
import os
import shlex
import shutil
import subprocess
from typing import Any

from repowire.spawn_ownership import probe_tmux_pane


async def capture_spawn_registration_diagnostics(
    *,
    pane_id: str | None,
    command: str | None,
    env: dict[str, str] | None,
) -> dict[str, Any]:
    """Capture bounded evidence for a spawned peer registration timeout."""
    return await asyncio.to_thread(
        _capture_spawn_registration_diagnostics,
        pane_id=pane_id,
        command=command,
        env=env,
    )


def _capture_spawn_registration_diagnostics(
    *,
    pane_id: str | None,
    command: str | None,
    env: dict[str, str] | None,
) -> dict[str, Any]:
    diagnostics: dict[str, Any] = {
        "pane_id": pane_id,
        "command": command,
    }
    path = (env or {}).get("PATH") or os.environ.get("PATH") or ""
    diagnostics["path"] = path
    command_head = _command_head(command)
    if command_head:
        diagnostics["command_head"] = command_head
        diagnostics["command_resolved"] = shutil.which(command_head, path=path)

    if pane_id:
        evidence = probe_tmux_pane(pane_id)
        diagnostics["pane_live"] = evidence is not None
        if evidence is not None:
            diagnostics["tmux_session"] = evidence.tmux_session
            diagnostics["pane_current_path"] = evidence.current_path
            diagnostics["pane_pid"] = evidence.pane_pid
        diagnostics["pane_tail"] = _capture_pane_tail(pane_id)
    else:
        diagnostics["pane_live"] = None
    return diagnostics


def _command_head(command: str | None) -> str | None:
    if not command:
        return None
    try:
        parts = shlex.split(command)
    except ValueError:
        return None
    if not parts:
        return None
    if parts[0] != "env":
        return parts[0]
    for part in parts[1:]:
        if "=" not in part:
            return part
    return None


def _capture_pane_tail(pane_id: str, *, max_chars: int = 4000) -> str | None:
    try:
        result = subprocess.run(
            ["tmux", "capture-pane", "-pt", pane_id, "-S", "-80"],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    tail = result.stdout.strip()
    if len(tail) > max_chars:
        return tail[-max_chars:]
    return tail
