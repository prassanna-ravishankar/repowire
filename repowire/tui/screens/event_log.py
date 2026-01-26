"""Event log screen - communication history."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from textual.app import ComposeResult
from textual.screen import Screen
from textual.widgets import DataTable, Static

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp


class EventLogScreen(Screen):
    """Screen showing communication event history."""

    BINDINGS = [
        ("escape", "back", "Back"),
        ("q", "back", "Back"),
        ("r", "refresh", "Refresh"),
        ("j", "cursor_down", "Down"),
        ("k", "cursor_up", "Up"),
    ]

    DEFAULT_CSS = """
    EventLogScreen {
        layout: vertical;
    }

    #title-bar {
        dock: top;
        height: 1;
        background: $primary;
        color: $text;
        text-align: center;
    }

    #event-table {
        height: 1fr;
    }

    #help-bar {
        dock: bottom;
        height: 1;
        background: $surface;
        color: $text-muted;
    }
    """

    def compose(self) -> ComposeResult:
        yield Static("EVENT LOG", id="title-bar")
        yield DataTable(id="event-table", cursor_type="row", zebra_stripes=True)
        yield Static(" [b][r][/]efresh  [b][q][/]/[b][esc][/] back", id="help-bar")

    @property
    def rw_app(self) -> RepowireApp:
        """Get typed app reference."""
        from repowire.tui.app import RepowireApp

        assert isinstance(self.app, RepowireApp)
        return self.app

    async def on_mount(self) -> None:
        """Set up table columns and load data."""
        table = self.query_one("#event-table", DataTable)
        table.add_column("Time", width=10)
        table.add_column("Type", width=12)
        table.add_column("From", width=12)
        table.add_column("To", width=12)
        table.add_column("Status", width=10)
        table.add_column("Text", width=40)

        await self.action_refresh()

    async def action_refresh(self) -> None:
        """Refresh event list."""
        events = await self.rw_app.daemon.get_events()
        self._update_table(events)

    def _update_table(self, events: list[dict[str, Any]]) -> None:
        """Update the event table."""
        table = self.query_one("#event-table", DataTable)
        table.clear()

        # Show most recent first
        for event in reversed(events):
            time_str = self._format_time(event.get("timestamp", ""))
            event_type = event.get("type", "?")
            from_peer = event.get("from", "-")
            to_peer = event.get("to", "-")
            status = event.get("status", "-")
            text = self._truncate(event.get("text", ""), 38)

            # Color-code event types
            type_display = self._format_type(event_type)
            status_display = self._format_status(status)

            table.add_row(time_str, type_display, from_peer, to_peer, status_display, text)

    def _format_time(self, timestamp: str) -> str:
        """Format timestamp to HH:MM:SS."""
        if not timestamp:
            return "-"
        # Parse ISO format and extract time
        try:
            if "T" in timestamp:
                time_part = timestamp.split("T")[1][:8]
                return time_part
        except (IndexError, ValueError):
            pass
        return timestamp[:10]

    def _format_type(self, event_type: str) -> str:
        """Format event type with color."""
        colors = {
            "query": "[cyan]query[/]",
            "response": "[green]response[/]",
            "notification": "[yellow]notify[/]",
            "broadcast": "[magenta]broadcast[/]",
            "status_change": "[blue]status[/]",
        }
        return colors.get(event_type, event_type)

    def _format_status(self, status: str) -> str:
        """Format status with color."""
        if status == "success":
            return "[green]ok[/]"
        elif status == "error":
            return "[red]err[/]"
        elif status == "pending":
            return "[yellow]...[/]"
        return status

    def _truncate(self, text: str, max_len: int) -> str:
        """Truncate text with ellipsis."""
        # Remove newlines
        text = text.replace("\n", " ").strip()
        if len(text) <= max_len:
            return text
        return text[: max_len - 3] + "..."

    def action_back(self) -> None:
        """Go back to main screen."""
        self.app.pop_screen()

    def action_cursor_down(self) -> None:
        """Move cursor down."""
        table = self.query_one("#event-table", DataTable)
        table.action_cursor_down()

    def action_cursor_up(self) -> None:
        """Move cursor up."""
        table = self.query_one("#event-table", DataTable)
        table.action_cursor_up()
