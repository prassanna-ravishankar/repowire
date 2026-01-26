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
            "[bold cyan]↵[/]open  "
            "[bold cyan]s[/]pawn  "
            "[bold cyan]k[/]ill  "
            "[bold cyan]o[/]ffline  "
            "[bold cyan]/[/]filter  "
            "[bold cyan]e[/]vents  "
            "[bold cyan]c[/]ircle  "
            "[bold cyan]q[/]uit"
        )
        return f" {keys}  │  {self.online}/{self.total} online"
