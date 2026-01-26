"""Peer details widget - shows detailed info about selected peer."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from textual.reactive import reactive
from textual.widgets import Static

from repowire.tui.services.daemon_client import PeerInfo


class PeerDetails(Static):
    """Panel showing detailed information about the selected peer."""

    peer: reactive[PeerInfo | None] = reactive(None)

    def __init__(self, **kwargs: Any) -> None:
        kwargs.setdefault("id", "peer-details")
        super().__init__(**kwargs)
        self.border_title = "Details"

    def watch_peer(self) -> None:
        """React to peer changes."""
        self._refresh_content()

    def _refresh_content(self) -> None:
        """Update the displayed content."""
        if self.peer is None:
            self.update(self._render_no_selection())
        else:
            self.update(self._render_peer(self.peer))

    def _render_no_selection(self) -> str:
        """Render placeholder when no peer selected."""
        return "[dim]Select a peer to view details[/]"

    def _render_peer(self, peer: PeerInfo) -> str:
        """Render peer details."""
        # Status icon and color
        status_lower = peer.status.lower()
        status_icons = {"online": "●", "busy": "◉", "offline": "○"}
        status_colors = {"online": "#9ece6a", "busy": "#e0af68", "offline": "#565f89"}
        status_icon = status_icons.get(status_lower, "?")
        status_color = status_colors.get(status_lower, "#565f89")

        # Build the details view
        lines = []

        # Header: Peer name (bold)
        lines.append(f"[bold #c0caf5]{peer.display_name.upper()}[/]")

        # Status line: ● ONLINE | claudemux
        lines.append(
            f"[{status_color}]{status_icon} {peer.status.upper()}[/] | [#7dcfff]{peer.backend}[/]"
        )

        lines.append("")  # Spacer

        # Circle
        lines.append(f"[#565f89]Circle:[/]    [#c0caf5]{peer.circle}[/]")

        # Path (truncate if too long)
        path = peer.path or "N/A"
        if len(path) > 35:
            path = "..." + path[-32:]
        lines.append(f"[#565f89]Path:[/]      [#c0caf5]{path}[/]")

        # Branch from metadata
        branch = peer.metadata.get("branch", "N/A")
        lines.append(f"[#565f89]Branch:[/]    [#bb9af7]{branch}[/]")

        # Last seen
        last_seen = self._format_last_seen(peer.metadata.get("last_seen"))
        lines.append(f"[#565f89]Last seen:[/] [#c0caf5]{last_seen}[/]")

        # Machine (if available)
        machine = peer.metadata.get("machine")
        if machine:
            lines.append(f"[#565f89]Machine:[/]   [#c0caf5]{machine}[/]")

        return "\n".join(lines)

    def _format_last_seen(self, timestamp: str | None) -> str:
        """Format last seen timestamp as relative time."""
        if not timestamp:
            return "N/A"

        try:
            ts = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            now = datetime.now(ts.tzinfo)
            delta = now - ts

            if delta.total_seconds() < 60:
                return "just now"
            elif delta.total_seconds() < 3600:
                mins = int(delta.total_seconds() / 60)
                return f"{mins} min ago"
            elif delta.total_seconds() < 86400:
                hours = int(delta.total_seconds() / 3600)
                return f"{hours} hour{'s' if hours > 1 else ''} ago"
            else:
                days = int(delta.total_seconds() / 86400)
                return f"{days} day{'s' if days > 1 else ''} ago"
        except (ValueError, TypeError):
            return "N/A"
