"""Agent list widget - simplified peer list grouped by circle."""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass

from textual.message import Message
from textual.reactive import reactive
from textual.widgets import OptionList
from textual.widgets.option_list import Option

from repowire.tui.services.daemon_client import PeerInfo

STATUS_SYMBOLS = {"online": "●", "busy": "◉", "offline": "○"}
STATUS_COLORS = {"online": "green", "busy": "yellow", "offline": "dim"}


@dataclass
class AgentSelected(Message):
    """Emitted when an agent is selected for inline detail view."""

    peer: PeerInfo | None


class AgentList(OptionList):
    """Displays agents grouped by circle with status indicators."""

    BINDINGS = [
        ("j", "cursor_down", "Down"),
        ("k", "cursor_up", "Up"),
    ]

    agents: reactive[list[PeerInfo]] = reactive(list, init=False)  # type: ignore[assignment]

    def __init__(self, **kwargs) -> None:
        super().__init__(**kwargs)
        self._id_to_peer: dict[str, PeerInfo] = {}

    def watch_agents(self) -> None:
        self._rebuild()

    def _rebuild(self) -> None:
        """Rebuild option list from current agents."""
        # Remember selection
        prev_id = self._selected_option_id()
        self.clear_options()
        self._id_to_peer.clear()

        peers = sorted(
            self.agents,
            key=lambda p: (
                p.circle or "global",
                {"online": 0, "busy": 1}.get(p.status.lower(), 2),
            ),
        )

        # Group by circle
        circles: dict[str, list[PeerInfo]] = defaultdict(list)
        for p in peers:
            circles[p.circle or "global"].append(p)

        for circle_name in sorted(circles):
            self.add_option(
                Option(f"── {circle_name} ──", id=f"__circle_{circle_name}", disabled=True)
            )
            for p in circles[circle_name]:
                sym = STATUS_SYMBOLS.get(p.status.lower(), "?")
                color = STATUS_COLORS.get(p.status.lower(), "")
                path_part = f"  [dim]{p.path}[/]" if p.path else ""
                label = f"[{color}]{sym}[/] {p.display_name}{path_part}"
                oid = f"agent_{p.peer_id}"
                self.add_option(Option(label, id=oid))
                self._id_to_peer[oid] = p

        # Restore selection
        if prev_id:
            for idx, opt in enumerate(self._options):
                if opt.id == prev_id:
                    self.highlighted = idx
                    break

    def _selected_option_id(self) -> str | None:
        if self.highlighted is None:
            return None
        try:
            opt = self.get_option_at_index(self.highlighted)
        except IndexError:
            return None
        return str(opt.id) if opt.id else None

    def _peer_for_option(self, option: Option) -> PeerInfo | None:
        oid = str(option.id) if option.id else ""
        return self._id_to_peer.get(oid)

    def on_option_list_option_highlighted(self, event: OptionList.OptionHighlighted) -> None:
        self.post_message(AgentSelected(peer=self._peer_for_option(event.option)))

    def on_option_list_option_selected(self, event: OptionList.OptionSelected) -> None:
        self.post_message(AgentSelected(peer=self._peer_for_option(event.option)))

    @property
    def selected_agent(self) -> PeerInfo | None:
        oid = self._selected_option_id()
        return self._id_to_peer.get(oid) if oid else None
