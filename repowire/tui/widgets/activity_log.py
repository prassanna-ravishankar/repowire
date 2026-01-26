"""Activity log widget - uses Textual's RichLog for conversation feed."""

from __future__ import annotations

from datetime import datetime

from textual import work
from textual.reactive import reactive
from textual.widgets import RichLog

from repowire.tui.services.daemon_client import Conversation, Event
from repowire.tui.services.sse_stream import SSEStream


class ActivityLog(RichLog):
    """Conversation feed showing query/response pairs."""

    MAX_EVENTS = 100

    filter_peer: reactive[str | None] = reactive(None)  # None = show all

    def __init__(self, base_url: str = "http://127.0.0.1:8377", **kwargs) -> None:
        super().__init__(highlight=True, markup=True, **kwargs)
        self._base_url = base_url
        self._sse: SSEStream | None = None
        self._events: list[Event] = []

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

        if not convos:
            if self.filter_peer:
                self.write(f"[dim]No conversations with {self.filter_peer}[/]")
            else:
                self.write("[dim]No conversations yet...[/]")
            return

        for c in convos[:10]:  # Show last 10 conversations
            self._write_conversation(c)

    def _write_conversation(self, c: Conversation) -> None:
        """Write a single conversation to the log."""
        time_str = self._format_time(c.timestamp)
        status_icons = {"pending": "...", "success": "[green]ok[/]", "error": "[red]err[/]"}
        status_icon = status_icons.get(c.status, "?")

        # Header line
        self.write(f"{time_str}  {c.from_peer} -> {c.to_peer}  {status_icon}")

        # Query text
        q_text = c.query.text[:45] + "..." if len(c.query.text) > 45 else c.query.text
        self.write(f'   [cyan]Q:[/] "{q_text}"')

        # Response text
        if c.response:
            r_text = (
                c.response.text[:45] + "..."
                if len(c.response.text) > 45
                else c.response.text
            )
            self.write(f'   [green]A:[/] "{r_text}"')
        elif c.status == "pending":
            self.write("   [dim]awaiting response...[/]")
        else:
            self.write("   [red]error[/]")

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
        self._sse = SSEStream(self._base_url)

        try:
            async for data in self._sse.stream_events():
                event = Event.from_dict(data)
                self._events.append(event)
                if len(self._events) > self.MAX_EVENTS:
                    self._events = self._events[-self.MAX_EVENTS:]
                self._refresh_content()
        except Exception:
            pass  # Stream stopped
