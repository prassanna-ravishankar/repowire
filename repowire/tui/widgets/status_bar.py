"""Status bar widget - footer with keybinds and stats."""

from __future__ import annotations

from textual.reactive import reactive
from textual.widget import Widget

KEYS = "[bold #7dcfff]s[/]hell  [bold #7dcfff]r[/]efresh  [bold #7dcfff]q[/]uit"


class StatusBar(Widget):
    """Footer status bar with peer stats."""

    online: reactive[int] = reactive(0)
    total: reactive[int] = reactive(0)

    def render(self) -> str:
        return f" {KEYS}  │  {self.online}/{self.total} online"
