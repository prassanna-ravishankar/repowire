"""Conversation modal - shows full conversation with Markdown rendering."""

from __future__ import annotations

from datetime import datetime

from textual.app import ComposeResult
from textual.containers import Vertical, VerticalScroll
from textual.screen import ModalScreen
from textual.widgets import Markdown, Static

from repowire.tui.services.daemon_client import Conversation


class ConversationScreen(ModalScreen[None]):
    """Modal screen showing full conversation details with Markdown."""

    BINDINGS = [
        ("escape", "close", "Close"),
        ("q", "close", "Close"),
    ]

    DEFAULT_CSS = """
    ConversationScreen {
        align: center middle;
    }

    #conversation-dialog {
        width: 80%;
        height: auto;
        max-width: 100;
        min-height: 15;
        max-height: 40;
        border: solid $primary;
        padding: 1 2;
    }

    #conversation-title {
        text-align: center;
        text-style: bold;
        margin-bottom: 1;
    }

    #conversation-meta {
        text-style: dim;
        margin-bottom: 1;
        padding-bottom: 1;
        border-bottom: solid $surface;
    }

    #conversation-content {
        height: 1fr;
        padding: 0 1;
    }

    .query-section {
        margin-bottom: 1;
    }

    .query-label {
        text-style: bold;
        margin-bottom: 0;
    }

    .response-section {
        margin-top: 1;
    }

    .response-label {
        text-style: bold;
        margin-bottom: 0;
    }

    .pending-response {
        text-style: dim italic;
    }

    .error-response {
        color: $error;
    }
    """

    def __init__(self, conversation: Conversation, **kwargs) -> None:
        super().__init__(**kwargs)
        self._conversation = conversation

    def compose(self) -> ComposeResult:
        c = self._conversation
        time_str = self._format_time(c.timestamp)
        status_colors = {"pending": "#e0af68", "success": "#9ece6a", "error": "#f7768e"}
        status_color = status_colors.get(c.status, "#565f89")

        with Vertical(id="conversation-dialog"):
            yield Static(f"{c.from_peer} -> {c.to_peer}", id="conversation-title")
            yield Static(
                f"[#565f89]{time_str}[/]  [{status_color}]{c.status.upper()}[/]",
                id="conversation-meta",
            )

            with VerticalScroll(id="conversation-content"):
                # Query section
                yield Static("[#7dcfff bold]Query[/]", classes="query-label")
                yield Markdown(c.query.text, classes="query-section")

                # Response section
                yield Static("[#9ece6a bold]Response[/]", classes="response-label")
                if c.response:
                    yield Markdown(c.response.text, classes="response-section")
                elif c.status == "pending":
                    yield Static(
                        "[italic]Awaiting response...[/]",
                        classes="pending-response",
                    )
                else:
                    yield Static(
                        "[#f7768e]Error: No response received[/]",
                        classes="error-response",
                    )

    def _format_time(self, timestamp: str) -> str:
        """Format timestamp as readable date/time."""
        if not timestamp:
            return "Unknown time"
        try:
            ts = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            return ts.astimezone().strftime("%Y-%m-%d %H:%M:%S")
        except (ValueError, TypeError):
            return "Unknown time"

    def action_close(self) -> None:
        """Close the modal."""
        self.dismiss(None)
