"""Status bar widget - footer with keybinds and stats using reactive attributes."""

from __future__ import annotations

from textual.reactive import reactive
from textual.widget import Widget


class StatusBar(Widget):
    """Footer status bar with keybindings and stats."""

    online: reactive[int] = reactive(0)
    total: reactive[int] = reactive(0)

    def render(self) -> str:
        """Render the status bar content."""
        keys = (
            "[bold #7dcfff]↵[/]conv  "
            "[bold #7dcfff]n[/]ew  "
            "[bold #7dcfff]s[/]hell  "
            "[bold #7dcfff]k[/]ill  "
            "[bold #7dcfff]o[/]ffline  "
            "[bold #7dcfff]/[/]filter  "
            "[bold #7dcfff]e[/]vents  "
            "[bold #7dcfff]c[/]ircle  "
            "[bold #7dcfff]q[/]uit"
        )
        return f" {keys}  │  {self.online}/{self.total} online"
