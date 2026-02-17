"""Main Textual application for Repowire TUI."""

from __future__ import annotations

import logging
import subprocess

from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.events import Key
from textual.screen import ModalScreen
from textual.widgets import Markdown, Static, TabbedContent, TabPane

from repowire.spawn import attach_session
from repowire.tui.services.daemon_client import Conversation, DaemonClient
from repowire.tui.widgets.agent_list import STATUS_COLORS, STATUS_SYMBOLS, AgentList, AgentSelected
from repowire.tui.widgets.communication_feed import CommunicationFeed, MessageSelected
from repowire.tui.widgets.create_agent_form import AgentCreated, CreateAgentForm
from repowire.tui.widgets.status_bar import StatusBar

logger = logging.getLogger(__name__)


class ConvoModal(ModalScreen):
    """Full-view modal for a query/response conversation."""

    BINDINGS = [("escape", "dismiss", "Close")]

    def __init__(self, conversation: Conversation, **kwargs) -> None:
        super().__init__(**kwargs)
        self._conversation = conversation

    def compose(self) -> ComposeResult:
        c = self._conversation
        with Vertical(id="conversation-dialog"):
            yield Static(
                f"[bold #7dcfff]{c.from_peer}[/] \u2192 [bold #7dcfff]{c.to_peer}[/]",
                id="conversation-title",
            )
            yield Markdown(f"**Q:** {c.query.text}")
            if c.response:
                yield Markdown(f"**R:** {c.response.text}")
            else:
                yield Static("[dim]Awaiting response...[/]")


class RepowireApp(App):
    """Repowire Agent Mesh Viewer."""

    TITLE = "Repowire Mesh"
    CSS_PATH = "styles.tcss"

    BINDINGS = [
        Binding("q", "quit", "Quit"),
        Binding("r", "refresh", "Refresh"),
        Binding("s", "shell", "Shell"),
    ]

    def __init__(self, daemon_url: str = "http://127.0.0.1:8377", **kwargs) -> None:
        super().__init__(**kwargs)
        self._daemon_url = daemon_url
        self._daemon: DaemonClient | None = None

    @property
    def daemon(self) -> DaemonClient:
        if self._daemon is None:
            raise RuntimeError("Daemon client not initialized")
        return self._daemon

    def compose(self) -> ComposeResult:
        with TabbedContent("Agents", "Communications", "Create", id="tabs"):
            with TabPane("Agents", id="tab-agents"):
                with Vertical(id="agents-pane"):
                    yield AgentList(id="agent-list")
                    yield Vertical(id="agent-detail")
            with TabPane("Communications", id="tab-comms"):
                yield CommunicationFeed(base_url=self._daemon_url, id="comm-feed")
            with TabPane("Create", id="tab-create"):
                yield CreateAgentForm(id="create-form")
        yield StatusBar()

    async def on_mount(self) -> None:
        self._daemon = DaemonClient(self._daemon_url)
        await self._daemon.__aenter__()

        health = await self._daemon.health()
        if health is None:
            self.notify(
                "Cannot connect to daemon. Run 'repowire serve' first.",
                severity="error",
                timeout=5,
            )
            self.set_timer(2, self.exit)
            return

        await self._load_peers()
        self.set_interval(5, self._load_peers)

    def on_key(self, event: Key) -> None:
        """Handle arrow keys from tabs to move focus to peer list."""
        # If tabs have focus and user presses up/down, move focus to agent list
        tabs = self.query_one(TabbedContent)
        if self.focused == tabs and event.key in ("up", "down"):
            agent_list = self.query_one("#agent-list", AgentList)
            agent_list.focus()
            event.prevent_default()
            event.stop()

    async def _load_peers(self) -> None:
        """Fetch peers from daemon and update widgets."""
        peers = await self.daemon.get_peers()
        agent_list = self.query_one("#agent-list", AgentList)
        agent_list.agents = peers

        # Update status bar counts
        bar = self.query_one(StatusBar)
        bar.total = len(peers)
        bar.online = sum(1 for p in peers if p.status.lower() == "online")

        # Update circles in create form
        circles = sorted({p.circle for p in peers if p.circle}) or ["default"]
        form = self.query_one("#create-form", CreateAgentForm)
        form.update_circles(circles)

    # -- Agent detail inline --

    def on_agent_selected(self, event: AgentSelected) -> None:
        detail = self.query_one("#agent-detail", Vertical)
        detail.remove_children()
        if event.peer is None:
            return
        p = event.peer
        status_key = p.status.lower()
        sym = STATUS_SYMBOLS.get(status_key, "?")
        color = STATUS_COLORS.get(status_key, "")
        lines = [
            f"[{color}]{sym}[/] [bold]{p.display_name}[/]  [dim]{p.peer_id}[/]",
            f"  circle: [magenta]{p.circle}[/]  agent: {p.backend}",
        ]
        if p.path:
            lines.append(f"  path: [dim]{p.path}[/]")
        if p.tmux_session:
            lines.append(f"  tmux: [dim]{p.tmux_session}[/]")
        detail.mount(Static("\n".join(lines)))

    # -- Message detail modal --

    def on_message_selected(self, event: MessageSelected) -> None:
        self.push_screen(ConvoModal(event.conversation))

    # -- Agent created --

    async def on_agent_created(self, event: AgentCreated) -> None:
        self.notify(f"Agent {event.display_name} created in {event.circle}")
        tabs = self.query_one(TabbedContent)
        tabs.active = "tab-agents"
        await self._load_peers()

    # -- Shell attach --

    async def action_shell(self) -> None:
        agent_list = self.query_one("#agent-list", AgentList)
        peer = agent_list.selected_agent
        if peer is None:
            self.notify("No agent selected", severity="warning")
            return
        if not peer.tmux_session:
            self.notify(
                f"Agent '{peer.name}' has no tmux session (agent type: {peer.backend})",
                severity="warning",
                timeout=3,
            )
            return
        with self.suspend():
            try:
                attach_session(peer.tmux_session)
            except subprocess.CalledProcessError as e:
                logger.debug(f"Attach ended: exit code {e.returncode}")
            except Exception as e:
                self.notify(f"Failed to attach: {e}", severity="error")
        await self._load_peers()

    async def action_refresh(self) -> None:
        await self._load_peers()

    async def on_unmount(self) -> None:
        if self._daemon:
            await self._daemon.__aexit__(None, None, None)
            self._daemon = None


def run_tui(daemon_url: str = "http://127.0.0.1:8377") -> None:
    """Run the TUI application."""
    app = RepowireApp(daemon_url=daemon_url)
    app.run()


if __name__ == "__main__":
    run_tui()
