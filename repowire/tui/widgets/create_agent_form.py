"""Create agent form widget for spawning new peers."""

from __future__ import annotations

import logging
from dataclasses import dataclass
from pathlib import Path

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.message import Message
from textual.widget import Widget
from textual.widgets import Button, Input, Label, Select

from repowire.config.models import AgentType
from repowire.spawn import SpawnConfig, spawn_peer

logger = logging.getLogger(__name__)

AGENT_TYPE_OPTIONS = [("Claude Code", "claude-code"), ("OpenCode", "opencode")]


@dataclass
class AgentCreated(Message):
    """Posted when a new agent is successfully spawned."""

    display_name: str
    circle: str


class CreateAgentForm(Widget):
    """Form for spawning a new peer agent."""

    DEFAULT_CSS = """
    CreateAgentForm {
        height: auto;
        padding: 1 2;
    }
    .form-field { height: 3; margin-bottom: 1; }
    .form-field Label { width: 14; content-align: right middle; text-style: dim; padding-right: 1; }
    .form-field Input { width: 1fr; }
    .form-field Select { width: 1fr; }
    #new-circle-input { display: none; }
    #form-buttons { margin-top: 1; align: right middle; }
    #form-buttons Button { margin-left: 1; }
    """

    def __init__(self, circles: list[str] | None = None, **kwargs) -> None:
        super().__init__(**kwargs)
        self._circles = circles or ["default"]

    def compose(self) -> ComposeResult:
        with Vertical():
            with Horizontal(classes="form-field"):
                yield Label("Agent Name")
                yield Input(placeholder="my-agent", id="name-input")
            with Horizontal(classes="form-field"):
                yield Label("Project Path")
                yield Input(placeholder="~/git/myproject", id="path-input")
            with Horizontal(classes="form-field"):
                yield Label("Circle")
                circle_opts: list[tuple[str, str]] = [(c, c) for c in self._circles]
                circle_opts.append(("+ New circle...", "__new__"))
                yield Select(circle_opts, value="default", id="circle-select")
            with Horizontal(classes="form-field"):
                yield Label("")
                yield Input(placeholder="Enter new circle name", id="new-circle-input")
            with Horizontal(classes="form-field"):
                yield Label("Agent Type")
                yield Select(AGENT_TYPE_OPTIONS, value="claude-code", id="backend-select")
            with Horizontal(id="form-buttons"):
                yield Button("Cancel", id="cancel-btn", variant="default")
                yield Button("Create Agent", id="create-btn", variant="success")

    def on_select_changed(self, event: Select.Changed) -> None:
        if event.select.id == "circle-select":
            new_input = self.query_one("#new-circle-input", Input)
            if event.value == "__new__":
                new_input.display = True
                new_input.focus()
            else:
                new_input.display = False

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id == "create-btn":
            self._do_create()
        elif event.button.id == "cancel-btn":
            self.reset()

    def _do_create(self) -> None:
        path = self.query_one("#path-input", Input).value.strip()
        if not path:
            self.notify("Project path is required", severity="error")
            return

        expanded = Path(path).expanduser()
        if not expanded.exists():
            self.notify(f"Path not found: {path}", severity="error")
            return

        circle_select = self.query_one("#circle-select", Select)
        if circle_select.value == "__new__":
            circle = self.query_one("#new-circle-input", Input).value.strip()
            if not circle:
                self.notify("Enter a circle name", severity="error")
                return
        else:
            circle = str(circle_select.value) if circle_select.value else "default"

        backend = str(self.query_one("#backend-select", Select).value) or "claude-code"
        command = "claude" if backend == "claude-code" else "opencode"

        # Use agent name as custom command prefix if provided (sets window name)
        config = SpawnConfig(
            path=str(expanded.resolve()),
            circle=circle,
            backend=AgentType(backend),
            command=command,
        )

        try:
            result = spawn_peer(config)
            self.notify(f"Spawned {result.display_name}")
            self.post_message(AgentCreated(display_name=result.display_name, circle=circle))
            self.reset()
        except (ValueError, RuntimeError) as e:
            self.notify(f"Spawn failed: {e}", severity="error")

    def reset(self) -> None:
        """Clear all form fields."""
        for input_id in ("#name-input", "#path-input", "#new-circle-input"):
            self.query_one(input_id, Input).value = ""
        self.query_one("#new-circle-input", Input).display = False
        self.query_one("#circle-select", Select).value = "default"
        self.query_one("#backend-select", Select).value = "claude-code"

    def update_circles(self, circles: list[str]) -> None:
        """Update available circles from daemon."""
        self._circles = circles
        opts: list[tuple[str, str]] = [(c, c) for c in circles]
        opts.append(("+ New circle...", "__new__"))
        select = self.query_one("#circle-select", Select)
        select.set_options(opts)
        if "default" in circles:
            select.value = "default"
