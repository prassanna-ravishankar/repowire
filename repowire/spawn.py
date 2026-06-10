"""Core spawn functionality for creating new peer sessions."""

from __future__ import annotations

import os
import shlex
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, cast
from uuid import uuid4

import libtmux
from libtmux.exc import LibTmuxException, ObjectDoesNotExist

from repowire.agent_backends import DEFAULT_SPAWN_COMMANDS
from repowire.agent_types import AgentType
from repowire.spawn_hints import write_hint

# Default commands for each agent type
AGENT_COMMANDS: dict[AgentType, str] = dict(DEFAULT_SPAWN_COMMANDS)


@dataclass
class SpawnConfig:
    """Configuration for spawning a new peer."""

    path: str
    circle: str
    backend: AgentType
    command: str = ""  # Full command to run (e.g., "claude --model opus")
    message: str | None = None  # Optional warmup intent passed to post_spawn_warmup
    role: str | None = None  # Optional peer role (e.g. "orchestrator"); default agent
    peer_id: str | None = None  # Optional same-id reconnect hint for durable restart
    env: dict[str, str] = field(default_factory=dict)  # Explicit env overlay for launched cmd

    @property
    def display_name(self) -> str:
        """Derive display name from path."""
        return Path(self.path).name


@dataclass
class SpawnResult:
    """Result of spawning a peer."""

    display_name: str
    tmux_session: str  # e.g., "circle:name"
    pane_id: str  # tmux pane id (e.g. "%42") for post-spawn warmup
    message: str | None = None  # Echo of the warmup intent (None = use default)


def spawn_peer(config: SpawnConfig) -> SpawnResult:
    """Spawn a new peer in a tmux window.

    Registration happens automatically via WebSocket when the agent starts.

    Args:
        config: Spawn configuration

    Returns:
        SpawnResult with display_name and tmux_session

    Raises:
        ValueError: If agent type is unknown
        RuntimeError: If tmux operations fail
    """
    server = libtmux.Server()
    display_name = config.display_name

    # Get or create session (circle = tmux session name). When the session is
    # new, create its first window as the target project window; otherwise tmux
    # creates an extra default window using the daemon's cwd.
    session, created_session = _get_or_create_session(
        server,
        config.circle,
        start_directory=config.path,
        window_name=display_name,
    )

    if created_session:
        window_name = display_name
        window = _find_window(session, window_name)
    else:
        # Find unique window name (append suffix if needed)
        window_name = _unique_window_name(session, display_name)
        # Create window with working directory
        window = session.new_window(window_name=window_name, start_directory=config.path)

    if window is None:
        raise RuntimeError("Failed to get tmux window")
    window = cast(Any, window)
    pane = window.active_pane

    if pane is None:
        raise RuntimeError("Failed to get active pane")

    # Determine command to run
    if config.command:
        cmd = config.command
    elif config.backend in AGENT_COMMANDS:
        cmd = AGENT_COMMANDS[config.backend]
    else:
        raise ValueError(f"Unknown agent type: {config.backend}")

    # Drop a hint so runtimes that strip tmux env (codex) can still discover
    # the requested circle when their MCP/hook subprocess registers. Role is
    # included for spawns that need a non-default role (e.g. orchestrator).
    write_hint(
        config.path,
        config.backend.value,
        config.circle,
        role=config.role,
        peer_id=config.peer_id,
        pending_first_turn=bool(config.message),
    )

    pane.send_keys(_command_with_env(cmd, config.env), enter=True)

    tmux_session = f"{config.circle}:{window_name}"

    return SpawnResult(
        display_name=window_name,
        tmux_session=tmux_session,
        pane_id=pane.id or "",
        message=config.message,
    )


def _get_or_create_session(
    server: libtmux.Server,
    session_name: str,
    *,
    start_directory: str | None = None,
    window_name: str | None = None,
) -> tuple[libtmux.Session, bool]:
    """Get existing session or create new one."""
    try:
        session = server.sessions.get(session_name=session_name)
        if session:
            return session, False
    except (LibTmuxException, ObjectDoesNotExist):
        pass

    return (
        server.new_session(
            session_name=session_name,
            start_directory=start_directory,
            window_name=window_name,
        ),
        True,
    )


def _find_window(session: libtmux.Session, window_name: str) -> Any | None:
    """Find a session window by name, falling back to the active/first window."""
    for window in session.windows:
        if getattr(window, "name", None) == window_name:
            return window

    active_window = getattr(session, "active_window", None)
    if active_window is not None:
        return active_window

    return next(iter(session.windows), None)


def _unique_window_name(session: libtmux.Session, base_name: str) -> str:
    """Generate unique window name, appending suffix if needed."""
    existing_names = {w.name for w in session.windows if w.name}

    if base_name not in existing_names:
        return base_name

    # Find next available suffix
    i = 2
    while f"{base_name}-{i}" in existing_names:
        i += 1
    return f"{base_name}-{i}"


# Above this, the typed line is replaced with a short launcher script. A fresh
# pane's line editor does not reliably accept a very long single paste: a
# captured login PATH with plugin bin dirs pushed the env-prefixed command past
# ~1.5k chars and it arrived truncated, so the agent never launched (the pane
# executed a PATH fragment instead).
_MAX_TYPED_COMMAND_CHARS = 512

_LAUNCHER_DIR = Path.home() / ".repowire" / "spawn-launchers"
_LAUNCHER_MAX_AGE_SECONDS = 24 * 3600


def _command_with_env(command: str, env: dict[str, str]) -> str:
    """Prefix a tmux shell command with explicit environment assignments.

    When the composed line is too long to type reliably, it is written to a
    launcher script and the typed line becomes a short ``sh <script>``.
    """
    clean_env = {key: value for key, value in env.items() if key and value is not None}
    if not clean_env:
        return command
    assignments = " ".join(
        f"{key}={shlex.quote(str(value))}" for key, value in sorted(clean_env.items())
    )
    line = f"env {assignments} {command}"
    if len(line) <= _MAX_TYPED_COMMAND_CHARS:
        return line
    return f"sh {shlex.quote(str(_write_launcher(line)))}"


def _write_launcher(line: str) -> Path:
    """Persist an over-long launch line as a script; prune stale siblings."""
    _LAUNCHER_DIR.mkdir(parents=True, exist_ok=True)
    cutoff = time.time() - _LAUNCHER_MAX_AGE_SECONDS
    for stale in _LAUNCHER_DIR.glob("launch-*.sh"):
        try:
            if stale.stat().st_mtime < cutoff:
                stale.unlink()
        except OSError:
            pass
    script = _LAUNCHER_DIR / f"launch-{uuid4().hex[:8]}.sh"
    script.write_text(f"#!/bin/sh\nexec {line}\n", encoding="utf-8")
    os.chmod(script, 0o700)
    return script


def attach_session(tmux_session: str) -> None:
    """Attach to a tmux session (blocks until detach)."""
    if ":" in tmux_session:
        session_name, window_name = tmux_session.split(":", 1)
        target = f"{session_name}:{window_name}"
    else:
        target = tmux_session

    subprocess.run(["tmux", "select-window", "-t", target], check=False)
    subprocess.run(["tmux", "attach-session", "-t", target.split(":")[0]], check=True)


def kill_peer(tmux_session: str) -> bool:
    """Kill a tmux window by session:window string.

    Window-name based, so vulnerable to renames. Prefer kill_pane(pane_id)
    when a stable pane id is available.
    """
    if ":" not in tmux_session:
        return False

    session_name, window_name = tmux_session.split(":", 1)
    server = libtmux.Server()

    try:
        session = server.sessions.get(session_name=session_name)
        if session is None:
            return False

        window = session.windows.get(window_name=window_name)
        if window is None:
            return False

        window.kill()
        return True
    except (LibTmuxException, ObjectDoesNotExist):
        return False


def kill_pane(pane_id: str) -> bool:
    """Kill a tmux pane by stable pane id (e.g. "%42").

    Pane ids survive window renames, so this is the preferred kill handle
    for daemon-spawned peers.
    """
    if not pane_id:
        return False
    try:
        result = subprocess.run(
            ["tmux", "kill-pane", "-t", pane_id],
            capture_output=True,
            text=True,
            check=False,
        )
        return result.returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False
