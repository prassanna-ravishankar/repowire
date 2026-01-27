"""Main screen - two-pane k9s-style layout."""

from __future__ import annotations

from typing import TYPE_CHECKING

from textual import work
from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.screen import Screen
from textual.widgets import Input

from repowire.spawn import kill_peer
from repowire.tui.services.daemon_client import PeerInfo
from repowire.tui.widgets.activity_log import ActivityLog, ConversationSelected
from repowire.tui.widgets.peer_details import PeerDetails
from repowire.tui.widgets.peer_list import PeerList, PeerSelected
from repowire.tui.widgets.status_bar import StatusBar

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp


class MainScreen(Screen):
    """Main screen with two-pane layout: peers left, details+conversations right."""

    BINDINGS = [
        ("q", "quit", "Quit"),
        ("n", "spawn", "New"),
        ("s", "shell", "Shell"),
        ("tab", "focus_conversations", "Conversations"),
        ("k", "kill", "Kill"),
        ("e", "events", "Events"),
        ("c", "circle", "Circle"),
        ("r", "refresh", "Refresh"),
        ("/", "filter", "Filter"),
        ("escape", "clear_focus", "Back"),
    ]

    def __init__(self, **kwargs) -> None:
        super().__init__(**kwargs)
        self._peers: list[PeerInfo] = []
        self._filter_text: str = ""
        self._filter_visible: bool = False

    def compose(self) -> ComposeResult:
        with Horizontal(id="main-content"):
            yield PeerList(id="peer-list")
            with Vertical(id="right-pane"):
                yield PeerDetails(id="peer-details")
                yield ActivityLog(id="activity-log")
        yield Input(placeholder="Filter peers...", id="filter-input")
        yield StatusBar(id="status-bar")

    @property
    def rw_app(self) -> RepowireApp:
        """Get typed app reference."""
        from repowire.tui.app import RepowireApp

        assert isinstance(self.app, RepowireApp)
        return self.app

    def on_mount(self) -> None:
        """Load initial data when screen mounts."""
        self.load_peers()
        # Set border title for peer list (only bordered element)
        self.query_one("#peer-list", PeerList).border_title = "Peers"

    def on_descendant_focus(self, event) -> None:
        """Update status bar context when focus changes."""
        status_bar = self.query_one("#status-bar", StatusBar)
        widget = event.widget

        if isinstance(widget, Input):
            status_bar.context = "filter"
        elif isinstance(widget, ActivityLog) and widget.nav_mode:
            status_bar.context = "conversations"
        else:
            status_bar.context = "peers"

    @work
    async def load_peers(self) -> None:
        """Load peers in background."""
        self._peers = await self.rw_app.daemon.get_peers()
        self._update_display()

    async def action_refresh(self) -> None:
        """Refresh peer list."""
        self._peers = await self.rw_app.daemon.get_peers()
        self._update_display()

    def _update_display(self) -> None:
        """Update widgets with current data."""
        peer_list = self.query_one("#peer-list", PeerList)
        status_bar = self.query_one("#status-bar", StatusBar)

        # Apply filter
        if self._filter_text:
            filtered = [
                p
                for p in self._peers
                if self._filter_text.lower() in p.name.lower()
                or self._filter_text.lower() in p.circle.lower()
            ]
        else:
            filtered = self._peers

        # Attributes down to children
        peer_list.peers = filtered

        # Update status bar stats (BUSY counts as online)
        online = sum(1 for p in self._peers if p.status.lower() in ("online", "busy"))
        status_bar.online = online
        status_bar.total = len(self._peers)

    def on_peer_selected(self, message: PeerSelected) -> None:
        """Handle peer selection from PeerList (messages up)."""
        # Update activity log filter
        activity_log = self.query_one("#activity-log", ActivityLog)
        activity_log.filter_peer = message.name  # None = all, str = filter

        # Update peer details panel
        peer_details = self.query_one("#peer-details", PeerDetails)
        if message.name:
            # Find the full peer info
            peer = next((p for p in self._peers if p.name == message.name), None)
            peer_details.peer = peer
        else:
            peer_details.peer = None

    async def _attach_to_peer(self, tmux_session: str) -> None:
        """Attach to peer's tmux session."""
        await self.rw_app.attach_to_peer(tmux_session)

    def action_spawn(self) -> None:
        """Open spawn modal."""
        from repowire.tui.screens.spawn import SpawnScreen

        self.app.push_screen(SpawnScreen())

    async def action_shell(self) -> None:
        """Attach to selected peer's tmux session (shell)."""
        peer_list = self.query_one("#peer-list", PeerList)
        peer = peer_list.get_selected_peer()

        if not peer:
            self.notify("No peer selected", severity="warning")
            return

        if not peer.tmux_session:
            self.notify(f"No tmux session for {peer.name}", severity="warning")
            return

        await self.rw_app.attach_to_peer(peer.tmux_session)

    def action_focus_conversations(self) -> None:
        """Focus the conversation log for navigation."""
        activity_log = self.query_one("#activity-log", ActivityLog)
        status_bar = self.query_one("#status-bar", StatusBar)
        activity_log.focus()
        activity_log.enter_nav_mode()
        status_bar.context = "conversations"

    def action_clear_focus(self) -> None:
        """Clear filter or exit navigation mode."""
        activity_log = self.query_one("#activity-log", ActivityLog)
        status_bar = self.query_one("#status-bar", StatusBar)

        # If activity log is focused and in nav mode, exit nav mode
        if activity_log.has_focus and activity_log.nav_mode:
            activity_log.exit_nav_mode()
            self.query_one("#peer-list", PeerList).focus()
            status_bar.context = "peers"
            return

        # Otherwise handle filter
        if self._filter_visible:
            filter_input = self.query_one("#filter-input", Input)
            filter_input.remove_class("visible")
            filter_input.value = ""
            self._filter_text = ""
            self._filter_visible = False
            self._update_display()
            status_bar.context = "peers"

    async def action_kill(self) -> None:
        """Kill selected peer's tmux window."""
        peer_list = self.query_one("#peer-list", PeerList)
        peer = peer_list.get_selected_peer()

        if not peer:
            self.notify("No peer selected", severity="warning")
            return

        if not peer.tmux_session:
            self.notify(f"No tmux session for {peer.name}", severity="warning")
            return

        # Confirm before killing
        from repowire.tui.screens.confirm import ConfirmScreen

        def on_confirm(confirmed: bool | None) -> None:
            if confirmed:
                self.rw_app.call_later(self._do_kill, peer.tmux_session, peer.name)

        self.app.push_screen(
            ConfirmScreen(f"Kill peer '{peer.name}'?"),
            on_confirm,
        )

    async def _do_kill(self, tmux_session: str, peer_name: str) -> None:
        """Actually kill the peer."""
        success = kill_peer(tmux_session)
        if success:
            await self.rw_app.daemon.unregister_peer(peer_name)
            self.notify(f"Killed {peer_name}")
            await self.action_refresh()
        else:
            self.notify(f"Failed to kill {peer_name}", severity="error")

    def action_events(self) -> None:
        """Show event log screen."""
        from repowire.tui.screens.event_log import EventLogScreen

        self.app.push_screen(EventLogScreen())

    def action_circle(self) -> None:
        """Change selected peer's circle."""
        peer_list = self.query_one("#peer-list", PeerList)
        peer = peer_list.get_selected_peer()

        if not peer:
            self.notify("No peer selected", severity="warning")
            return

        from repowire.tui.screens.circle import CircleScreen

        self.app.push_screen(CircleScreen(peer.name))

    def action_filter(self) -> None:
        """Show filter input."""
        filter_input = self.query_one("#filter-input", Input)
        status_bar = self.query_one("#status-bar", StatusBar)
        filter_input.add_class("visible")
        filter_input.focus()
        self._filter_visible = True
        status_bar.context = "filter"

    def action_quit(self) -> None:
        """Quit the application."""
        self.app.exit()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        """Handle filter input submission."""
        if event.input.id == "filter-input":
            self._filter_text = event.value
            self._update_display()
            event.input.remove_class("visible")
            self._filter_visible = False
            status_bar = self.query_one("#status-bar", StatusBar)
            status_bar.context = "peers"

    def on_conversation_selected(self, message: ConversationSelected) -> None:
        """Handle conversation selection from ActivityLog."""
        from repowire.tui.screens.conversation import ConversationScreen

        self.app.push_screen(ConversationScreen(message.conversation))
