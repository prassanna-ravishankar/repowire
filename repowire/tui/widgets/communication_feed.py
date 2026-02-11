"""Communication feed widget - real-time SSE stream of peer messages."""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from datetime import datetime

from textual import work
from textual.message import Message
from textual.reactive import reactive
from textual.widgets import RichLog

from repowire.tui.services.daemon_client import Conversation, Event
from repowire.tui.services.sse_stream import SSEStream

logger = logging.getLogger(__name__)

MAX_EVENTS = 200
MAX_DISPLAY = 50

CONVO_STATUS_INDICATORS = {
    "pending": "[dim]...[/]",
    "success": "[#9ece6a]\u2713[/]",
    "error": "[#f7768e]\u2717[/]",
}


@dataclass
class MessageSelected(Message):
    """Posted when a conversation is selected for full-view modal."""

    conversation: Conversation


class CommunicationFeed(RichLog):
    """Real-time communication stream between peers.

    Displays query/response pairs, broadcasts, and notifications
    from the daemon SSE event stream with auto-scroll.
    """

    filter_peer: reactive[str | None] = reactive(None)
    selected_index: reactive[int] = reactive(0)
    nav_mode: reactive[bool] = reactive(False)

    BINDINGS = [
        ("j", "nav_down", "Down"),
        ("k", "nav_up", "Up"),
        ("enter", "select", "View"),
    ]

    def __init__(self, base_url: str = "http://127.0.0.1:8377", **kwargs) -> None:
        super().__init__(highlight=True, markup=True, auto_scroll=True, **kwargs)
        self._base_url = base_url
        self._sse: SSEStream | None = None
        self._events: list[Event] = []
        self._conversations: list[Conversation] = []
        self._broadcasts: list[Event] = []

    def on_mount(self) -> None:
        self._render_feed()
        self._start_stream()

    def on_unmount(self) -> None:
        if self._sse:
            self._sse.stop()

    def watch_filter_peer(self) -> None:
        self._render_feed()

    def watch_selected_index(self) -> None:
        if self.nav_mode:
            self._render_feed()

    def watch_nav_mode(self) -> None:
        self._render_feed()

    # -- Navigation --

    def action_nav_down(self) -> None:
        if self.nav_mode and self._conversations:
            self.selected_index = min(self.selected_index + 1, len(self._conversations) - 1)

    def action_nav_up(self) -> None:
        if self.nav_mode and self._conversations:
            self.selected_index = max(self.selected_index - 1, 0)

    def action_select(self) -> None:
        if self.nav_mode and 0 <= self.selected_index < len(self._conversations):
            self.post_message(MessageSelected(self._conversations[self.selected_index]))

    # -- Rendering --

    def _render_feed(self) -> None:
        self.clear()

        # Build conversations from events
        convos = Conversation.from_events(list(self._events))
        broadcasts = [e for e in self._events if e.type == "broadcast"]

        # Apply peer filter
        if self.filter_peer:
            convos = [
                c
                for c in convos
                if c.from_peer == self.filter_peer or c.to_peer == self.filter_peer
            ]
            broadcasts = [b for b in broadcasts if b.from_peer == self.filter_peer]

        self._conversations = convos[:MAX_DISPLAY]
        self._broadcasts = broadcasts

        if not self._conversations and not self._broadcasts:
            self.write("[dim]Waiting for messages...[/]")
            return

        # Clamp selection
        if self.selected_index >= len(self._conversations):
            self.selected_index = max(0, len(self._conversations) - 1)

        # Build index lookup for selected conversation
        convo_indices: dict[str, int] = {c.id: i for i, c in enumerate(self._conversations)}

        # Merge and sort all items by timestamp (newest last for auto-scroll)
        items: list[tuple[str, Conversation | Event]] = [
            (c.timestamp, c) for c in self._conversations
        ] + [(b.timestamp, b) for b in self._broadcasts]
        items.sort(key=lambda x: x[0])

        for _, item in items:
            if isinstance(item, Conversation):
                idx = convo_indices[item.id]
                is_selected = self.nav_mode and idx == self.selected_index
                self._write_conversation(item, selected=is_selected)
            else:
                self._write_broadcast(item)

    def _write_conversation(self, c: Conversation, selected: bool = False) -> None:
        ts = _format_time(c.timestamp)
        arrow = "\u2192"  # →
        indicator = CONVO_STATUS_INDICATORS.get(c.status, "")
        sel = "[#bb9af7]\u25b6[/] " if selected else "  "

        peers = f"[bold]{c.from_peer}[/] {arrow} [bold]{c.to_peer}[/]"
        header = f"{sel}[dim]{ts}[/]  {peers}  {indicator}"
        if selected:
            self.write(f"[on #333333]{header}[/]")
        else:
            self.write(header)

        # Query
        q_text = _truncate(c.query.text, 60)
        self.write(f"    [#7dcfff]Q:[/] {q_text}")

        # Response
        if c.response:
            r_text = _truncate(c.response.text, 60)
            self.write(f"    [#9ece6a]R:[/] {r_text}")
        elif c.status == "pending":
            self.write("    [dim italic]awaiting response...[/]")

        self.write("")

    def _write_broadcast(self, event: Event) -> None:
        ts = _format_time(event.timestamp)
        sender = event.from_peer or "?"
        text = _truncate(event.text, 60)
        self.write(f"  [dim]{ts}[/]  [#e0af68][bold]{sender}[/] \u00bb broadcast[/]")
        self.write(f"    [#e0af68]{text}[/]")
        self.write("")

    # -- SSE Stream --

    @work(exclusive=True)
    async def _start_stream(self) -> None:
        self._sse = SSEStream(self._base_url)
        try:
            async for data in self._sse.stream_events():
                event = Event.from_dict(data)
                self._events.append(event)
                if len(self._events) > MAX_EVENTS:
                    self._events = self._events[-MAX_EVENTS:]
                self._render_feed()
        except asyncio.CancelledError:
            pass
        except Exception as e:
            logger.warning(f"SSE stream error: {e}")


def _format_time(timestamp: str) -> str:
    """Format ISO timestamp as 12-hour time like '2:34pm'."""
    if not timestamp:
        return "??:??"
    try:
        ts = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
        local = ts.astimezone()
        hour = local.hour % 12 or 12
        minute = local.strftime("%M")
        ampm = "am" if local.hour < 12 else "pm"
        return f"{hour}:{minute}{ampm}"
    except (ValueError, TypeError):
        return "??:??"


def _truncate(text: str, length: int) -> str:
    """Truncate text with ellipsis."""
    return text[:length] + "\u2026" if len(text) > length else text
