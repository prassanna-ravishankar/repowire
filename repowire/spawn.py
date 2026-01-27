"""Core spawn functionality for creating new peer sessions."""

from __future__ import annotations

import subprocess
from dataclasses import dataclass
from pathlib import Path

import httpx
import libtmux
from libtmux.exc import LibTmuxException, ObjectDoesNotExist

from repowire.config.models import BackendType
from repowire.daemon.deps import get_config

# Default commands for each backend
BACKEND_COMMANDS: dict[BackendType, str] = {"claudemux": "claude", "opencode": "opencode"}


@dataclass
class SpawnConfig:
    """Configuration for spawning a new peer."""

    path: str
    circle: str
    backend: BackendType  # "claudemux" or "opencode"
    command: str = ""  # Full command to run (e.g., "claude --model opus")

    @property
    def display_name(self) -> str:
        """Derive display name from path."""
        return Path(self.path).name


@dataclass
class SpawnResult:
    """Result of spawning a peer."""

    pane_id: str  # e.g., "%42"
    display_name: str
    tmux_session: str  # e.g., "circle:name"
    registered: bool = False  # Whether daemon registration succeeded


def spawn_peer(config: SpawnConfig) -> SpawnResult:
    """Spawn a new peer in a tmux window and register with daemon.

    Args:
        config: Spawn configuration

    Returns:
        SpawnResult with pane_id, display_name, tmux_session, and registered status

    Raises:
        ValueError: If backend is unknown
        RuntimeError: If tmux operations fail
    """
    server = libtmux.Server()
    display_name = config.display_name

    # Get or create session (circle = tmux session name)
    session = _get_or_create_session(server, config.circle)

    # Find unique window name (append suffix if needed)
    window_name = _unique_window_name(session, display_name)

    # Create window with working directory
    window = session.new_window(window_name=window_name, start_directory=config.path)
    pane = window.active_pane

    if pane is None:
        raise RuntimeError("Failed to get active pane")

    # Determine command to run
    if config.command:
        cmd = config.command
    elif config.backend in BACKEND_COMMANDS:
        cmd = BACKEND_COMMANDS[config.backend]
    else:
        raise ValueError(f"Unknown backend: {config.backend}")

    pane.send_keys(cmd, enter=True)

    tmux_session = f"{config.circle}:{window_name}"

    # Register with daemon
    registered = _register_with_daemon(
        pane_id=pane.id or "",
        display_name=window_name,
        path=config.path,
        tmux_session=tmux_session,
        circle=config.circle,
        backend=config.backend,
    )

    return SpawnResult(
        pane_id=pane.id or "",
        display_name=window_name,
        tmux_session=tmux_session,
        registered=registered,
    )


def _get_or_create_session(server: libtmux.Server, session_name: str) -> libtmux.Session:
    """Get existing session or create new one."""
    try:
        session = server.sessions.get(session_name=session_name)
        if session:
            return session
    except (LibTmuxException, ObjectDoesNotExist):
        pass

    return server.new_session(session_name=session_name)


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


def _register_with_daemon(
    pane_id: str,
    display_name: str,
    path: str,
    tmux_session: str,
    circle: str,
    backend: BackendType,
) -> bool:
    """Register peer with daemon. Returns True if successful."""
    import logging

    logger = logging.getLogger(__name__)

    try:
        cfg = get_config()
        daemon_url = f"http://{cfg.daemon.host}:{cfg.daemon.port}"

        with httpx.Client(timeout=5.0) as client:
            resp = client.post(
                f"{daemon_url}/peers",
                json={
                    "pane_id": pane_id,
                    "display_name": display_name,
                    "name": display_name,  # Backward compatibility
                    "path": path,
                    "tmux_session": tmux_session,
                    "circle": circle,
                    "backend": backend,
                },
            )
            resp.raise_for_status()
        return True
    except httpx.HTTPStatusError as e:
        logger.warning(f"Daemon registration failed: {e.response.status_code}")
        return False
    except httpx.RequestError as e:
        logger.warning(f"Daemon not reachable: {e}")
        return False


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
    """Kill a tmux window. Returns True if successful."""
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


def list_tmux_sessions() -> list[str]:
    """List all tmux session names."""
    try:
        server = libtmux.Server()
        return [s.name for s in server.sessions if s.name]
    except LibTmuxException:
        return []
