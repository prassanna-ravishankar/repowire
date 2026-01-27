"""Activity log widget - uses Textual's RichLog for conversation feed."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

from textual import work
from textual.message import Message
from textual.reactive import reactive
from textual.widgets import RichLog

from repowire.tui.services.daemon_client import Conversation, Event
from repowire.tui.services.sse_stream import SSEStream


@dataclass
class ConversationSelected(Message):
    """Posted when a conversation is selected for viewing."""

    conversation: Conversation


class ActivityLog(RichLog):
    """Conversation feed showing query/response pairs with navigation mode."""

    MAX_EVENTS = 100

    filter_peer: reactive[str | None] = reactive(None)  # None = show all
    nav_mode: reactive[bool] = reactive(False)  # Navigation mode

    BINDINGS = [
        ("j", "nav_down", "Down"),
        ("k", "nav_up", "Up"),
        ("down", "nav_down", "Down"),
        ("up", "nav_up", "Up"),
        ("enter", "select_conversation", "View"),
    ]

    def __init__(self, base_url: str = "http://127.0.0.1:8377", **kwargs) -> None:
        super().__init__(highlight=True, markup=True, **kwargs)
        self._base_url = base_url
        self._sse: SSEStream | None = None
        self._events: list[Event] = []
        self._conversations: list[Conversation] = []
        self._selected_index: int = 0

    @property
    def events(self) -> list[Event]:
        """Get current events."""
        return self._events

    @events.setter
    def events(self, value: list[Event]) -> None:
        """Set events and re-render."""
        self._events = value
        self._refresh_content()

    def on_mount(self) -> None:
        """Start streaming events when mounted."""
        self._refresh_content()
        self.stream_events()

    def on_unmount(self) -> None:
        """Stop streaming when unmounted."""
        if self._sse:
            self._sse.stop()

    def watch_filter_peer(self) -> None:
        """React to filter changes."""
        self._refresh_content()

    def watch_nav_mode(self) -> None:
        """React to navigation mode changes."""
        if self.nav_mode:
            self.add_class("nav-mode")
        else:
            self.remove_class("nav-mode")
        self._refresh_content()

    def enter_nav_mode(self) -> None:
        """Enter navigation mode for j/k navigation."""
        self.nav_mode = True
        self._selected_index = 0

    def exit_nav_mode(self) -> None:
        """Exit navigation mode."""
        self.nav_mode = False

    def action_nav_down(self) -> None:
        """Move selection down."""
        if not self.nav_mode or not self._conversations:
            return
        self._selected_index = min(self._selected_index + 1, len(self._conversations) - 1)
        self._refresh_content()

    def action_nav_up(self) -> None:
        """Move selection up."""
        if not self.nav_mode or not self._conversations:
            return
        self._selected_index = max(self._selected_index - 1, 0)
        self._refresh_content()

    def action_select_conversation(self) -> None:
        """Select current conversation to view in modal."""
        if not self.nav_mode or not self._conversations:
            return
        if 0 <= self._selected_index < len(self._conversations):
            conversation = self._conversations[self._selected_index]
            self.post_message(ConversationSelected(conversation=conversation))

    def _refresh_content(self) -> None:
        """Update the displayed content."""
        self.clear()
        convos = Conversation.from_events(list(self._events))

        # Filter by peer if set
        if self.filter_peer:
            convos = [
                c
                for c in convos
                if c.from_peer == self.filter_peer or c.to_peer == self.filter_peer
            ]

        self._conversations = convos[:10]  # Keep last 10

        if not self._conversations:
            if self.filter_peer:
                self.write(f"[dim]No conversations with {self.filter_peer}[/]")
            else:
                self.write("[dim]No conversations yet...[/]")
            return

        # Ensure selected index is valid
        if self._selected_index >= len(self._conversations):
            self._selected_index = max(0, len(self._conversations) - 1)

        for i, c in enumerate(self._conversations):
            self._write_conversation(c, selected=(self.nav_mode and i == self._selected_index))

    def _write_conversation(self, c: Conversation, selected: bool = False) -> None:
        """Write a single conversation to the log."""
        time_str = self._format_time(c.timestamp)
        status_icons = {"pending": "...", "success": "[#9ece6a]ok[/]", "error": "[#f7768e]err[/]"}
        status_icon = status_icons.get(c.status, "?")

        # Selection indicator
        prefix = "[#bb9af7]>[/] " if selected else "  "

        # Header line
        header = f"{prefix}{time_str}  {c.from_peer} -> {c.to_peer}  {status_icon}"
        if selected:
            self.write(f"[reverse]{header}[/]")
        else:
            self.write(header)

        # Query text
        q_text = c.query.text[:45] + "..." if len(c.query.text) > 45 else c.query.text
        self.write(f'   [#7dcfff]Q:[/] "{q_text}"')

        # Response text
        if c.response:
            r_text = c.response.text[:45] + "..." if len(c.response.text) > 45 else c.response.text
            self.write(f'   [#9ece6a]A:[/] "{r_text}"')
        elif c.status == "pending":
            self.write("   [dim]awaiting response...[/]")
        else:
            self.write("   [#f7768e]error[/]")

        # Empty line between conversations
        self.write("")

    def _format_time(self, timestamp: str) -> str:
        """Format timestamp as HH:MM."""
        if not timestamp:
            return "??:??"
        try:
            ts = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            return ts.astimezone().strftime("%H:%M")
        except (ValueError, TypeError):
            return "??:??"

    @work(exclusive=True)
    async def stream_events(self) -> None:
        """Stream events from SSE in background."""
        import asyncio
        import logging

        logger = logging.getLogger(__name__)
        self._sse = SSEStream(self._base_url)

        try:
            async for data in self._sse.stream_events():
                event = Event.from_dict(data)
                self._events.append(event)
                if len(self._events) > self.MAX_EVENTS:
                    self._events = self._events[-self.MAX_EVENTS :]
                self._refresh_content()
        except asyncio.CancelledError:
            pass  # Normal shutdown
        except Exception as e:
            logger.warning(f"SSE stream stopped unexpectedly: {e}")
