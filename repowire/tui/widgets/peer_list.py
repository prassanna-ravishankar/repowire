"""Peer list widget - uses Textual's OptionList for k9s-style layout."""

from __future__ import annotations

import logging
from collections import defaultdict
from dataclasses import dataclass

from textual.css.query import NoMatches
from textual.message import Message
from textual.reactive import reactive
from textual.widgets import OptionList
from textual.widgets.option_list import Option

from repowire.tui.services.daemon_client import PeerInfo

logger = logging.getLogger(__name__)

# Status display constants
STATUS_SYMBOLS = {"online": "●", "busy": "◉", "offline": "○"}
STATUS_COLORS = {"online": "green", "busy": "yellow", "offline": "dim"}


@dataclass
class PeerSelected(Message):
    """Posted when a peer is selected."""

    name: str | None  # None = "All"
    tmux_session: str | None


class PeerList(OptionList):
    """Compact peer list with 'All' option and offline toggle."""

    BINDINGS = [
        ("j", "cursor_down", "Down"),
        ("k", "cursor_up", "Up"),
        ("o", "toggle_offline", "Offline"),
    ]

    show_offline: reactive[bool] = reactive(False, init=False)

    def __init__(self, **kwargs) -> None:
        super().__init__(**kwargs)
        self._all_peers: list[PeerInfo] = []
        self._option_to_peer: dict[str, PeerInfo] = {}  # option_id -> peer
        self._rebuilding = False  # Guard against recursive rebuild

    @property
    def peers(self) -> list[PeerInfo]:
        """Get current peers."""
        return self._all_peers

    @peers.setter
    def peers(self, value: list[PeerInfo]) -> None:
        """Set peers and re-render."""
        self._all_peers = value
        self._rebuild_options()

    @property
    def _visible_peers(self) -> list[PeerInfo]:
        """Get peers filtered by offline toggle, sorted by circle then status."""
        if self.show_offline:
            peers = self._all_peers
        else:
            peers = [p for p in self._all_peers if p.status.lower() != "offline"]
        # Sort by circle first, then by status within each circle
        return sorted(
            peers,
            key=lambda p: (
                p.circle or "global",
                {"online": 0, "busy": 1}.get(p.status.lower(), 2),
            ),
        )

    def watch_show_offline(self) -> None:
        """React to offline toggle."""
        if not self._rebuilding:
            self._rebuild_options()

    def _rebuild_options(self) -> None:
        """Rebuild the option list from current peers."""
        if self._rebuilding:
            return
        self._rebuilding = True
        try:
            # Preserve current selection
            selected_id: str | None = None
            if self.highlighted is not None:
                try:
                    option = self.get_option_at_index(self.highlighted)
                    selected_id = str(option.id) if option.id else None
                except (IndexError, NoMatches):
                    pass
                except Exception as e:
                    logger.debug(f"Failed to preserve selection: {e}")

            self.clear_options()
            self._option_to_peer.clear()

            peers = self._visible_peers
            online_count = sum(1 for p in self._all_peers if p.status.lower() in ("online", "busy"))

            # "All" option
            self.add_option(Option(f"All ({online_count})", id="__all__"))

            # Group peers by circle
            circles: dict[str, list[PeerInfo]] = defaultdict(list)
            for p in peers:
                circles[p.circle or "global"].append(p)

            # Render each circle group
            for circle_name in sorted(circles.keys()):
                circle_peers = circles[circle_name]
                # Separator (None) and circle header as disabled option
                self.add_option(None)  # Separator
                circle_id = f"__circle_{circle_name}__"
                circle_opt = Option(f"─ {circle_name} ─", id=circle_id, disabled=True)
                self.add_option(circle_opt)

                for p in circle_peers:
                    status_symbol = STATUS_SYMBOLS.get(p.status.lower(), "?")
                    status_color = STATUS_COLORS.get(p.status.lower(), "")
                    label = f"{p.name}  [{status_color}]{status_symbol}[/]"
                    option_id = f"peer_{p.name}"
                    self.add_option(Option(label, id=option_id))
                    self._option_to_peer[option_id] = p

            # Restore selection if possible
            if selected_id:
                for idx, opt in enumerate(self._options):
                    if opt.id == selected_id:
                        self.highlighted = idx
                        break
        finally:
            self._rebuilding = False

    def _handle_option_event(self, option_id: str) -> None:
        """Handle option selection or highlight by posting PeerSelected message."""
        if option_id == "__all__":
            self.post_message(PeerSelected(name=None, tmux_session=None))
        elif option_id.startswith("peer_"):
            peer = self._option_to_peer.get(option_id)
            if peer:
                self.post_message(PeerSelected(name=peer.name, tmux_session=peer.tmux_session))

    def on_option_list_option_selected(self, event: OptionList.OptionSelected) -> None:
        """Handle option selection."""
        option_id = str(event.option.id) if event.option.id else ""
        self._handle_option_event(option_id)

    def on_option_list_option_highlighted(self, event: OptionList.OptionHighlighted) -> None:
        """Handle option highlight (cursor movement)."""
        option_id = str(event.option.id) if event.option.id else ""
        self._handle_option_event(option_id)

    def action_toggle_offline(self) -> None:
        """Toggle showing offline peers."""
        self.show_offline = not self.show_offline

    def get_selected_peer(self) -> PeerInfo | None:
        """Get the currently selected peer (None if 'All' selected)."""
        if self.highlighted is None:
            return None
        option = self.get_option_at_index(self.highlighted)
        option_id = str(option.id) if option.id else ""
        if option_id.startswith("peer_"):
            return self._option_to_peer.get(option_id)
        return None

    def get_filter_peer_name(self) -> str | None:
        """Get name of selected peer for filtering (None = all)."""
        peer = self.get_selected_peer()
        return peer.name if peer else None
