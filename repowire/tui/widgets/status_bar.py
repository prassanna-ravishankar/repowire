"""Status bar widget - footer with keybinds and stats using reactive attributes."""

from __future__ import annotations

from textual.reactive import reactive
from textual.widget import Widget

# Keybinding sets for different contexts
KEYBINDINGS = {
    "peers": (
        "[bold #7dcfff]tab[/]conv  "
        "[bold #7dcfff]n[/]ew  "
        "[bold #7dcfff]s[/]hell  "
        "[bold #7dcfff]k[/]ill  "
        "[bold #7dcfff]o[/]ffline  "
        "[bold #7dcfff]/[/]filter  "
        "[bold #7dcfff]e[/]vents  "
        "[bold #7dcfff]c[/]ircle  "
        "[bold #7dcfff]q[/]uit"
    ),
    "conversations": (
        "[bold #7dcfff]j[/]/[bold #7dcfff]k[/]nav  "
        "[bold #7dcfff]enter[/]view  "
        "[bold #7dcfff]esc[/]back  "
        "[bold #7dcfff]q[/]uit"
    ),
    "filter": ("[bold #7dcfff]enter[/]apply  [bold #7dcfff]esc[/]cancel"),
}


class StatusBar(Widget):
    """Footer status bar with context-sensitive keybindings and stats."""

    online: reactive[int] = reactive(0)
    total: reactive[int] = reactive(0)
    context: reactive[str] = reactive("peers")

    def render(self) -> str:
        """Render the status bar content."""
        keys = KEYBINDINGS.get(self.context, KEYBINDINGS["peers"])
        return f" {keys}  │  {self.online}/{self.total} online"
