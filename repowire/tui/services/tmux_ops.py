"""Tmux operations for spawning, attaching, and killing peer sessions."""

from __future__ import annotations

import subprocess
from dataclasses import dataclass

import libtmux
from libtmux.exc import LibTmuxException, ObjectDoesNotExist


@dataclass
class SpawnConfig:
    """Configuration for spawning a new peer."""

    path: str
    circle: str
    backend: str  # "claudemux" or "opencode"
    command: str = ""  # Full command to run (e.g., "claude --model opus")

    @property
    def display_name(self) -> str:
        """Derive display name from path."""
        from pathlib import Path

        return Path(self.path).name


@dataclass
class SpawnResult:
    """Result of spawning a peer."""

    pane_id: str  # e.g., "%42"
    display_name: str
    tmux_session: str  # e.g., "circle:name"


class TmuxOps:
    """Tmux operations for managing peer sessions."""

    def __init__(self) -> None:
        self._server: libtmux.Server | None = None

    @property
    def server(self) -> libtmux.Server:
        if self._server is None:
            self._server = libtmux.Server()
        return self._server

    def get_or_create_session(self, session_name: str) -> libtmux.Session:
        """Get existing session or create new one."""
        try:
            session = self.server.sessions.get(session_name=session_name)
            if session:
                return session
        except (LibTmuxException, ObjectDoesNotExist):
            pass

        return self.server.new_session(session_name=session_name)

    def spawn_peer(self, config: SpawnConfig) -> SpawnResult:
        """Spawn a new peer in a tmux window.

        Args:
            config: Spawn configuration

        Returns:
            SpawnResult with pane_id, display_name, and tmux_session
        """
        display_name = config.display_name

        # Circle = tmux session name
        session = self.get_or_create_session(config.circle)

        # Check if window already exists
        try:
            existing = session.windows.get(window_name=display_name)
            if existing:
                raise ValueError(f"Window '{display_name}' already exists in session '{config.circle}'")
        except ObjectDoesNotExist:
            pass  # Window doesn't exist, which is what we want

        # Create window with working directory
        window = session.new_window(window_name=display_name, start_directory=config.path)
        pane = window.active_pane

        if pane is None:
            raise RuntimeError("Failed to get active pane")

        # Determine command to run
        if config.command:
            cmd = config.command
        elif config.backend == "claudemux":
            cmd = "claude"
        elif config.backend == "opencode":
            cmd = "opencode"
        else:
            raise ValueError(f"Unknown backend: {config.backend}")

        pane.send_keys(cmd, enter=True)

        return SpawnResult(
            pane_id=pane.id or "",
            display_name=display_name,
            tmux_session=f"{config.circle}:{display_name}",
        )

    def attach_session(self, tmux_session: str) -> None:
        """Attach to a tmux session (blocks until detach).

        This should be called after suspending the TUI.
        """
        # Parse session:window format
        if ":" in tmux_session:
            session_name, window_name = tmux_session.split(":", 1)
            target = f"{session_name}:{window_name}"
        else:
            target = tmux_session

        # Use subprocess to attach (this blocks)
        subprocess.run(["tmux", "select-window", "-t", target], check=False)
        subprocess.run(["tmux", "attach-session", "-t", target.split(":")[0]], check=True)

    def kill_window(self, tmux_session: str) -> bool:
        """Kill a tmux window.

        Args:
            tmux_session: Session:window target (e.g., "myteam:frontend")

        Returns:
            True if killed successfully
        """
        if ":" not in tmux_session:
            return False

        session_name, window_name = tmux_session.split(":", 1)

        try:
            session = self.server.sessions.get(session_name=session_name)
            if session is None:
                return False

            window = session.windows.get(window_name=window_name)
            if window is None:
                return False

            window.kill()
            return True
        except LibTmuxException:
            return False

    def list_sessions(self) -> list[str]:
        """List all tmux sessions."""
        try:
            return [s.name for s in self.server.sessions if s.name]
        except LibTmuxException:
            return []

    def window_exists(self, tmux_session: str) -> bool:
        """Check if a tmux window exists."""
        if ":" not in tmux_session:
            return False

        session_name, window_name = tmux_session.split(":", 1)

        try:
            session = self.server.sessions.get(session_name=session_name)
            if session is None:
                return False
            window = session.windows.get(window_name=window_name)
            return window is not None
        except LibTmuxException:
            return False
