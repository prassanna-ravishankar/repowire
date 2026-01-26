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

        # Header: Peer name (bold) + status inline
        lines.append(
            f"[bold #c0caf5]{peer.display_name.upper()}[/]  "
            f"[{status_color}]{status_icon}[/] [{status_color}]{peer.status.upper()}[/]"
        )

        # Backend + circle on same line
        lines.append(f"[#7dcfff]{peer.backend}[/] · [#565f89]circle:[/] [#c0caf5]{peer.circle}[/]")

        # Path (full, no truncation - widget handles overflow)
        if peer.path:
            lines.append(f"[#565f89]{peer.path}[/]")

        # Branch from metadata (only if present)
        branch = peer.metadata.get("branch")
        if branch:
            lines.append(f"[#bb9af7]⎇ {branch}[/]")

        # Last seen (only if present, use top-level field)
        last_seen = self._format_last_seen(peer.last_seen)
        if last_seen:
            lines.append(f"[dim]{last_seen}[/]")

        # Machine (only if present, use top-level field)
        if peer.machine:
            lines.append(f"[dim]@ {peer.machine}[/]")

        return "\n".join(lines)

    def _format_last_seen(self, timestamp: str | None) -> str:
        """Format last seen timestamp as relative time. Returns empty if not available."""
        if not timestamp:
            return ""

        try:
            ts = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            now = datetime.now(ts.tzinfo)
            delta = now - ts

            if delta.total_seconds() < 60:
                return "seen just now"
            elif delta.total_seconds() < 3600:
                mins = int(delta.total_seconds() / 60)
                return f"seen {mins}m ago"
            elif delta.total_seconds() < 86400:
                hours = int(delta.total_seconds() / 3600)
                return f"seen {hours}h ago"
            else:
                days = int(delta.total_seconds() / 86400)
                return f"seen {days}d ago"
        except (ValueError, TypeError):
            return ""
