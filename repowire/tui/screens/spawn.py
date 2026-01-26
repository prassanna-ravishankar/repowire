"""Spawn screen - modal form for spawning new peers."""

from __future__ import annotations

import os
from pathlib import Path
from typing import TYPE_CHECKING

from textual.app import ComposeResult
from textual.containers import Grid, Vertical
from textual.screen import ModalScreen
from textual.widgets import Button, Input, Label, Select, Static

from repowire.spawn import SpawnConfig, spawn_peer

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp

# Sentinel value for "Create new circle" option
CREATE_NEW_CIRCLE = "__create_new__"


class SpawnScreen(ModalScreen[bool]):
    """Modal screen for spawning a new peer."""

    BINDINGS = [
        ("escape", "cancel", "Cancel"),
    ]

    DEFAULT_CSS = """
    SpawnScreen {
        align: center middle;
    }

    #spawn-dialog {
        width: 60;
        height: auto;
        border: thick #7dcfff;
        background: #24283b;
        padding: 1 2;
    }

    #spawn-title {
        text-align: center;
        text-style: bold;
        color: #7dcfff;
        margin-bottom: 1;
    }

    .form-row {
        height: 3;
        margin-bottom: 1;
    }

    .form-row Label {
        width: 12;
        height: 1;
        content-align: right middle;
        padding-right: 1;
        color: #565f89;
    }

    .form-row Input, .form-row Select {
        width: 1fr;
    }

    #new-circle-row {
        display: none;
    }

    #new-circle-row.visible {
        display: block;
    }

    #button-row {
        height: 3;
        align: center middle;
        margin-top: 1;
    }

    #button-row Button {
        margin: 0 1;
    }
    """

    def __init__(self, **kwargs) -> None:
        super().__init__(**kwargs)
        self._circles: list[str] = []

    def compose(self) -> ComposeResult:
        with Vertical(id="spawn-dialog"):
            yield Static("Spawn New Peer", id="spawn-title")

            with Grid(classes="form-row"):
                yield Label("Path:")
                yield Input(
                    placeholder=str(Path.home()),
                    value=os.getcwd(),
                    id="path-input",
                )

            with Grid(classes="form-row"):
                yield Label("Backend:")
                yield Select(
                    [
                        ("Claude Code (claudemux)", "claudemux"),
                        ("OpenCode", "opencode"),
                    ],
                    value="claudemux",
                    id="backend-select",
                )

            with Grid(classes="form-row"):
                yield Label("Command:")
                yield Input(
                    placeholder="claude (auto-set based on backend)",
                    value="",
                    id="command-input",
                )

            with Grid(classes="form-row"):
                yield Label("Circle:")
                yield Select(
                    [("default", "default")],  # Will be populated on mount
                    value="default",
                    id="circle-select",
                )

            with Grid(classes="form-row", id="new-circle-row"):
                yield Label("New circle:")
                yield Input(
                    placeholder="Enter circle name",
                    value="",
                    id="new-circle-input",
                )

            with Grid(id="button-row"):
                yield Button("Spawn", variant="primary", id="spawn-btn")
                yield Button("Cancel", variant="default", id="cancel-btn")

    @property
    def rw_app(self) -> RepowireApp:
        """Get typed app reference."""
        from repowire.tui.app import RepowireApp

        assert isinstance(self.app, RepowireApp)
        return self.app

    async def on_mount(self) -> None:
        """Load circles on mount."""
        await self._load_circles()

    async def _load_circles(self) -> None:
        """Fetch existing circles from peers."""
        peers = await self.rw_app.daemon.get_peers()

        # Extract unique circles
        circles = set()
        for p in peers:
            if p.circle:
                circles.add(p.circle)

        # Always include 'default'
        circles.add("default")
        self._circles = sorted(circles)

        # Build select options
        options: list[tuple[str, str]] = [(c, c) for c in self._circles]
        options.append(("+ Create new...", CREATE_NEW_CIRCLE))

        # Update the select widget
        circle_select = self.query_one("#circle-select", Select)
        circle_select.set_options(options)

    def on_select_changed(self, event: Select.Changed) -> None:
        """Handle select changes."""
        if event.select.id == "circle-select":
            new_circle_row = self.query_one("#new-circle-row")
            if event.value == CREATE_NEW_CIRCLE:
                new_circle_row.add_class("visible")
                self.query_one("#new-circle-input", Input).focus()
            else:
                new_circle_row.remove_class("visible")

        elif event.select.id == "backend-select":
            # Update command placeholder based on backend
            command_input = self.query_one("#command-input", Input)
            if event.value == "claudemux":
                command_input.placeholder = "claude (auto-set based on backend)"
            elif event.value == "opencode":
                command_input.placeholder = "opencode (auto-set based on backend)"

    async def on_button_pressed(self, event: Button.Pressed) -> None:
        """Handle button presses."""
        if event.button.id == "cancel-btn":
            self.dismiss(False)
        elif event.button.id == "spawn-btn":
            await self._do_spawn()

    async def _do_spawn(self) -> None:
        """Spawn the peer."""
        path = self.query_one("#path-input", Input).value.strip()
        backend = self.query_one("#backend-select", Select).value
        command = self.query_one("#command-input", Input).value.strip()
        circle_value = self.query_one("#circle-select", Select).value

        # Validation
        if not path:
            path = os.getcwd()

        if not Path(path).exists():
            self.notify(f"Path does not exist: {path}", severity="error")
            return

        if backend is None or backend == Select.BLANK:
            self.notify("Backend is required", severity="error")
            return

        # Determine circle
        if circle_value == CREATE_NEW_CIRCLE:
            circle = self.query_one("#new-circle-input", Input).value.strip()
            if not circle:
                self.notify("Please enter a circle name", severity="error")
                return
        else:
            circle = str(circle_value) if circle_value else "default"

        # Default command based on backend
        if not command:
            command = "claude" if backend == "claudemux" else "opencode"

        config = SpawnConfig(
            path=str(Path(path).resolve()),
            circle=circle,
            backend=str(backend),
            command=command,
        )

        try:
            result = spawn_peer(config)
            msg = f"Spawned {result.display_name} in {circle}"
            if not result.registered:
                msg += " (daemon not running)"
            self.notify(msg)
            self.dismiss(True)
        except ValueError as e:
            self.notify(str(e), severity="error")
        except Exception as e:
            err_msg = str(e) if str(e) else type(e).__name__
            self.notify(f"Failed: {err_msg}", severity="error")

    def action_cancel(self) -> None:
        """Cancel and close modal."""
        self.dismiss(False)
