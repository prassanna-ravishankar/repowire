"""Spawn screen - minimal modal for spawning new peers."""

from __future__ import annotations

import os
from pathlib import Path
from typing import TYPE_CHECKING

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.screen import ModalScreen
from textual.suggester import Suggester
from textual.widgets import Button, Input, Label, Select, Static

from repowire.spawn import SpawnConfig, spawn_peer

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp


class PathSuggester(Suggester):
    """Directory autocomplete suggester."""

    async def get_suggestion(self, value: str) -> str | None:
        if not value:
            return None

        path = Path(value).expanduser()

        # Complete directory with trailing slash
        if path.is_dir() and not value.endswith("/"):
            return value + "/"

        # Find matching subdirectory
        if value.endswith("/"):
            parent = path
            partial = ""
        else:
            parent = path.parent
            partial = path.name.lower()

        if not parent.exists():
            return None

        try:
            for entry in sorted(parent.iterdir()):
                if entry.is_dir() and entry.name.lower().startswith(partial):
                    suggestion = str(parent / entry.name) + "/"
                    if suggestion != value:
                        return suggestion
        except PermissionError:
            pass

        return None


class SpawnScreen(ModalScreen[bool]):
    """Minimal modal for spawning a new peer."""

    BINDINGS = [
        Binding("escape", "cancel", "Cancel", show=False),
        Binding("enter", "submit", "Spawn", show=False),
        Binding("down", "focus_next", "Next field", show=False),
        Binding("up", "focus_previous", "Previous field", show=False),
        Binding("tab", "complete_path", "Autocomplete", show=False),
        Binding("shift+tab", "noop", "", show=False),
    ]

    DEFAULT_CSS = """
    SpawnScreen {
        align: center middle;
        background: rgba(0, 0, 0, 0.6);
    }

    #dialog {
        width: 60;
        height: 23;
        background: #1a1b26;
        border: round #414868;
        padding: 1 2;
    }

    #title {
        text-style: bold;
        color: #7dcfff;
        width: 100%;
        text-align: center;
        height: 2;
    }

    .field-label {
        color: #565f89;
        height: 1;
    }

    #path-input {
        height: 3;
        margin-bottom: 1;
    }

    #options-row {
        height: 3;
        margin-bottom: 2;
    }

    #backend-select {
        width: 1fr;
    }

    #circle-select {
        width: 1fr;
        margin-left: 2;
    }

    #command-input {
        height: 3;
    }

    #buttons {
        dock: bottom;
        width: 100%;
        height: 3;
        align: right middle;
    }

    #cancel-btn {
        margin-right: 1;
    }

    #spawn-btn {
        background: #9ece6a;
        color: #1a1b26;
    }

    #spawn-btn:hover {
        background: #73daca;
    }

    #spawn-btn:focus {
        text-style: bold reverse;
    }
    """

    def __init__(self, **kwargs) -> None:
        super().__init__(**kwargs)
        self._circles: list[str] = []

    def compose(self) -> ComposeResult:
        with Vertical(id="dialog"):
            yield Static("New Peer", id="title")

            yield Label("Path", classes="field-label")
            yield Input(
                value=os.getcwd(),
                placeholder="~/git/myproject",
                id="path-input",
                suggester=PathSuggester(use_cache=False),
            )

            with Horizontal(id="options-row"):
                yield Select(
                    [("Claude Code", "claudemux"), ("OpenCode", "opencode")],
                    value="claudemux",
                    id="backend-select",
                    prompt="Backend",
                )
                yield Select(
                    [("default", "default")],
                    value="default",
                    id="circle-select",
                    prompt="Circle",
                )

            yield Label("Command (optional)", classes="field-label")
            yield Input(
                placeholder="claude --dangerously-skip-permissions",
                id="command-input",
            )

            with Horizontal(id="buttons"):
                yield Button("Cancel", id="cancel-btn", variant="default")
                yield Button("Spawn", id="spawn-btn", variant="success")

    @property
    def rw_app(self) -> RepowireApp:
        from repowire.tui.app import RepowireApp
        assert isinstance(self.app, RepowireApp)
        return self.app

    async def on_mount(self) -> None:
        await self._load_circles()
        self.query_one("#path-input", Input).focus()

    async def _load_circles(self) -> None:
        peers = await self.rw_app.daemon.get_peers()

        circles = {"default"}
        for p in peers:
            if p.circle:
                circles.add(p.circle)

        self._circles = sorted(circles)

        # Build options with "new" at the end
        options: list[tuple[str, str]] = [(c, c) for c in self._circles]
        options.append(("+ New circle...", "__new__"))

        circle_select = self.query_one("#circle-select", Select)
        circle_select.set_options(options)
        circle_select.value = "default"

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id == "cancel-btn":
            self.dismiss(False)
        elif event.button.id == "spawn-btn":
            self._do_spawn()

    def on_select_changed(self, event: Select.Changed) -> None:
        if event.select.id == "circle-select" and event.value == "__new__":
            self.app.call_later(self._prompt_new_circle)

    async def _prompt_new_circle(self) -> None:
        """Replace circle select with input."""
        circle_select = self.query_one("#circle-select", Select)
        circle_select.display = False

        # Mount input in its place
        options_row = self.query_one("#options-row", Horizontal)
        new_input = Input(placeholder="Circle name", id="new-circle-input")
        new_input.styles.width = "1fr"
        await options_row.mount(new_input)
        new_input.focus()

    def action_cancel(self) -> None:
        self.dismiss(False)

    def action_submit(self) -> None:
        self._do_spawn()

    def action_focus_next(self) -> None:
        self.focus_next()

    def action_focus_previous(self) -> None:
        self.focus_previous()

    def action_complete_path(self) -> None:
        """Accept path autocomplete suggestion or move to next field."""
        try:
            path_input = self.query_one("#path-input", Input)
            if self.focused == path_input:
                # Check if there's a suggestion to accept
                suggestion = getattr(path_input, "_suggestion", "")
                if suggestion and suggestion != path_input.value:
                    path_input.value = suggestion
                    path_input.cursor_position = len(suggestion)
                    return
        except Exception:
            pass
        # No suggestion - move to next field
        self.focus_next()

    def action_noop(self) -> None:
        """Do nothing - blocks shift+tab navigation."""
        pass

    def _do_spawn(self) -> None:
        path = self.query_one("#path-input", Input).value.strip() or os.getcwd()

        expanded_path = Path(path).expanduser()
        if not expanded_path.exists():
            self.notify(f"Path not found: {path}", severity="error")
            return

        # Get backend
        backend_select = self.query_one("#backend-select", Select)
        backend = str(backend_select.value) if backend_select.value else "claudemux"

        # Get circle
        try:
            new_input = self.query_one("#new-circle-input", Input)
            circle = new_input.value.strip()
            if not circle:
                self.notify("Enter a circle name", severity="error")
                return
        except Exception:
            circle_select = self.query_one("#circle-select", Select)
            val = circle_select.value
            circle = str(val) if val and val != "__new__" else "default"

        # Get command
        command = self.query_one("#command-input", Input).value.strip()
        if not command:
            command = "claude" if backend == "claudemux" else "opencode"

        config = SpawnConfig(
            path=str(expanded_path.resolve()),
            circle=circle,
            backend=backend,
            command=command,
        )

        try:
            result = spawn_peer(config)
            self.notify(f"Spawned {result.display_name}")
            self.dismiss(True)
        except Exception as e:
            self.notify(f"Failed: {str(e) or type(e).__name__}", severity="error")
