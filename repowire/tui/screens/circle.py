"""Circle change screen - modal for changing peer's circle."""

from __future__ import annotations

from typing import TYPE_CHECKING

from textual.app import ComposeResult
from textual.containers import Grid, Vertical
from textual.screen import ModalScreen
from textual.widgets import Button, Input, Label, Static

if TYPE_CHECKING:
    from repowire.tui.app import RepowireApp


class CircleScreen(ModalScreen[bool]):
    """Modal screen for changing a peer's circle."""

    BINDINGS = [
        ("escape", "cancel", "Cancel"),
    ]

    DEFAULT_CSS = """
    CircleScreen {
        align: center middle;
    }

    #circle-dialog {
        width: 45;
        height: auto;
        border: solid $primary;
        padding: 1 2;
    }

    #circle-title {
        text-align: center;
        text-style: bold;
        margin-bottom: 1;
    }

    .form-row {
        grid-size: 2;
        grid-columns: 8 1fr;
        height: auto;
        margin-bottom: 1;
    }

    .form-row Label {
        text-style: dim;
        content-align: right middle;
    }

    #button-row {
        height: auto;
        align: center middle;
        margin-top: 1;
    }

    #button-row Button {
        margin: 0 1;
    }
    """

    def __init__(self, peer_name: str, **kwargs) -> None:
        super().__init__(**kwargs)
        self._peer_name = peer_name

    def compose(self) -> ComposeResult:
        with Vertical(id="circle-dialog"):
            yield Static(f"Change Circle: {self._peer_name}", id="circle-title")

            with Grid(classes="form-row"):
                yield Label("Circle:")
                yield Input(placeholder="circle-name", id="circle-input")

            with Grid(id="button-row"):
                yield Button("Save", variant="primary", id="save-btn")
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
        elif event.button.id == "save-btn":
            await self._do_save()

    async def _do_save(self) -> None:
        """Save the circle change."""
        circle = self.query_one("#circle-input", Input).value.strip()

        if not circle:
            self.notify("Circle name is required", severity="error")
            return

        success = await self.rw_app.daemon.set_peer_circle(self._peer_name, circle)
        if success:
            self.notify(f"Moved {self._peer_name} to circle '{circle}'")
            self.dismiss(True)
        else:
            self.notify(f"Failed to change circle for {self._peer_name}", severity="error")

    def action_cancel(self) -> None:
        """Cancel and close modal."""
        self.dismiss(False)
