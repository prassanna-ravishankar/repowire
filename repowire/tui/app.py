"""Main Textual application for Repowire TUI."""

from __future__ import annotations

import logging
import subprocess

from textual.app import App

from repowire.spawn import attach_session
from repowire.tui.screens.main import MainScreen
from repowire.tui.services.daemon_client import DaemonClient

logger = logging.getLogger(__name__)


class RepowireApp(App):
    """Repowire TUI - htop-style interface for managing Claude Code peers."""

    TITLE = "Repowire Top"
    CSS_PATH = "styles.tcss"

    def __init__(self, daemon_url: str = "http://127.0.0.1:8377", **kwargs) -> None:
        super().__init__(**kwargs)
        self._daemon_url = daemon_url
        self._daemon: DaemonClient | None = None

    @property
    def daemon(self) -> DaemonClient:
        """Get daemon client."""
        if self._daemon is None:
            raise RuntimeError("Daemon client not initialized")
        return self._daemon

    async def on_mount(self) -> None:
        """Initialize daemon client and check connection."""
        self._daemon = DaemonClient(self._daemon_url)
        await self._daemon.__aenter__()

        # Check daemon health
        health = await self._daemon.health()
        if health is None:
            self.notify(
                "Cannot connect to daemon. Run 'repowire serve' first.",
                severity="error",
                timeout=5,
            )
            # Exit after showing message
            self.set_timer(2, self.exit)
            return

        # Show main screen
        await self.push_screen(MainScreen())

        # Start periodic refresh
        self.set_interval(5, self._refresh_peers)

    async def _refresh_peers(self) -> None:
        """Periodically refresh peer list."""
        screen = self.screen
        if hasattr(screen, "action_refresh"):
            await screen.action_refresh()  # type: ignore[operator]

    async def on_unmount(self) -> None:
        """Clean up daemon client."""
        if self._daemon:
            await self._daemon.__aexit__(None, None, None)
            self._daemon = None

    async def attach_to_peer(self, tmux_session: str) -> None:
        """Suspend TUI and attach to tmux session."""
        # Suspend the TUI
        with self.suspend():
            # Attach to tmux (blocks until detach)
            try:
                attach_session(tmux_session)
            except subprocess.CalledProcessError as e:
                logger.debug(f"Attach to {tmux_session} ended: exit code {e.returncode}")

        # Refresh after returning
        if hasattr(self.screen, "action_refresh"):
            await self.screen.action_refresh()  # type: ignore[operator]


def run_tui(daemon_url: str = "http://127.0.0.1:8377") -> None:
    """Run the TUI application."""
    app = RepowireApp(daemon_url=daemon_url)
    app.run()


if __name__ == "__main__":
    run_tui()
