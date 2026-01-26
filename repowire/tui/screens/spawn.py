"""Spawn screen - modal form for spawning new peers."""

from __future__ import annotations

import os
from pathlib import Path
from typing import TYPE_CHECKING

from textual.app import ComposeResult
from textual.containers import Grid, Vertical
from textual.screen import ModalScreen
from textual.widgets import Button, Input, Label, Select, Static

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp


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
        border: thick $primary;
        background: $surface;
        padding: 1 2;
    }

    #spawn-title {
        text-align: center;
        text-style: bold;
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
    }

    .form-row Input, .form-row Select {
        width: 1fr;
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
        self._default_circle = "default"

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
                yield Label("Model:")
                yield Input(
                    placeholder="sonnet (optional)",
                    value="",
                    id="model-input",
                )

            with Grid(classes="form-row"):
                yield Label("Params:")
                yield Input(
                    placeholder="extra flags (optional)",
                    value="",
                    id="params-input",
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

    async def on_button_pressed(self, event: Button.Pressed) -> None:
        """Handle button presses."""
        if event.button.id == "cancel-btn":
            self.dismiss(False)
        elif event.button.id == "spawn-btn":
            await self._do_spawn()

    async def _do_spawn(self) -> None:
        """Spawn the peer."""
        from repowire.tui.services.tmux_ops import SpawnConfig

        path = self.query_one("#path-input", Input).value.strip()
        backend = self.query_one("#backend-select", Select).value
        model = self.query_one("#model-input", Input).value.strip()
        params = self.query_one("#params-input", Input).value.strip()

        # Validation
        if not path:
            path = os.getcwd()

        if not Path(path).exists():
            self.notify(f"Path does not exist: {path}", severity="error")
            return

        if backend is None or backend == Select.BLANK:
            self.notify("Backend is required", severity="error")
            return

        # Default model if not specified
        if not model:
            model = "sonnet"

        # Circle defaults to "default" for now
        circle = self._default_circle

        config = SpawnConfig(
            path=path,
            circle=circle,
            backend=str(backend),
            model=model,
            params=params,
        )

        try:
            result = self.rw_app.tmux.spawn_peer(config)

            # Register with daemon
            await self.rw_app.daemon.register_peer(
                name=result.display_name,
                path=path,
                tmux_session=result.tmux_session,
                circle=circle,
            )

            self.notify(f"Spawned {result.display_name} in {circle}")
            self.dismiss(True)
        except ValueError as e:
            self.notify(str(e), severity="error")
        except Exception as e:
            self.notify(f"Failed to spawn: {e}", severity="error")

    def action_cancel(self) -> None:
        """Cancel and close modal."""
        self.dismiss(False)
