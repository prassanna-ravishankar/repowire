from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING

from rich.text import Text
from textual import work
from textual.app import App, ComposeResult
from textual.containers import Horizontal, Vertical, VerticalScroll
from textual.reactive import reactive
from textual.widgets import Footer, Header, Input, RichLog, Static, Tree

if TYPE_CHECKING:
    from repowire.config import RepowireConfig
    from repowire.daemon import RepowireDaemon


COLORS = {
    "blue": "dodger_blue1",
    "green": "green3",
    "yellow": "yellow3",
    "red": "red3",
    "cyan": "cyan3",
    "magenta": "magenta3",
    "white": "white",
}


class AgentPane(Vertical):
    def __init__(self, agent_name: str, color: str, **kwargs) -> None:
        super().__init__(**kwargs)
        self.agent_name = agent_name
        self.color = COLORS.get(color, "white")

    def compose(self) -> ComposeResult:
        yield Static(
            f"[bold {self.color}]{self.agent_name}[/]",
            classes="pane-header",
        )
        yield RichLog(
            id=f"log-{self.agent_name}",
            highlight=True,
            markup=True,
            wrap=True,
            classes="agent-log",
        )


class StatusBar(Static):
    status_text = reactive("Initializing...")

    def render(self) -> Text:
        return Text(f" {self.status_text}", style="bold white on dark_blue")


class RepowireTUI(App):
    CSS = """
    Screen {
        layout: grid;
        grid-size: 1;
        grid-rows: auto 1fr auto auto auto;
    }
    
    #agent-container {
        height: 100%;
    }
    
    .agent-pane {
        width: 1fr;
        border: solid $accent;
        margin: 0 1;
    }
    
    .pane-header {
        height: 1;
        padding: 0 1;
        background: $surface;
        text-align: center;
    }
    
    .agent-log {
        height: 1fr;
        padding: 0 1;
    }
    
    #status-section {
        height: auto;
        max-height: 8;
        border: solid $primary;
        margin: 1;
    }
    
    #blackboard-section {
        height: auto;
        max-height: 6;
        border: solid $secondary;
        margin: 0 1;
        padding: 0 1;
    }
    
    #input-section {
        height: auto;
        padding: 1;
    }
    
    #command-input {
        width: 100%;
    }
    
    StatusBar {
        height: 1;
        dock: bottom;
    }
    
    Tree {
        height: auto;
        max-height: 6;
    }
    """

    BINDINGS = [
        ("ctrl+q", "quit", "Quit"),
        ("ctrl+r", "refresh", "Refresh"),
        ("ctrl+b", "toggle_blackboard", "Blackboard"),
        ("escape", "focus_input", "Focus Input"),
    ]

    def __init__(self, config: RepowireConfig, daemon: RepowireDaemon | None = None) -> None:
        super().__init__()
        self.config = config
        self.daemon = daemon
        self._agent_names = list(config.agents.keys())

    def compose(self) -> ComposeResult:
        yield Header(show_clock=True)

        with Horizontal(id="agent-container"):
            for name, agent_cfg in self.config.agents.items():
                yield AgentPane(
                    agent_name=name,
                    color=agent_cfg.color,
                    classes="agent-pane",
                    id=f"pane-{name}",
                )

        with VerticalScroll(id="status-section"):
            tree: Tree[str] = Tree("Agent Dependencies", id="dep-tree")
            tree.root.expand()
            yield tree

        yield Static("Blackboard: (empty)", id="blackboard-section")

        with Vertical(id="input-section"):
            yield Input(
                placeholder="Enter command to broadcast to all agents...",
                id="command-input",
            )

        yield StatusBar(id="status-bar")
        yield Footer()

    def on_mount(self) -> None:
        self._build_dependency_tree()
        self.set_interval(0.5, self._poll_agent_logs)
        self.set_interval(2.0, self._update_blackboard_display)
        self._update_status("Ready - Mesh: " + self.config.name)

        if self.daemon:
            self.run_worker(self._boot_daemon())

    @work(exclusive=True)
    async def _boot_daemon(self) -> None:
        if self.daemon:
            self._update_status("Booting agents...")
            await self.daemon.boot()
            self._update_status("Mesh active - " + self.config.name)

    def _build_dependency_tree(self) -> None:
        tree = self.query_one("#dep-tree", Tree)
        tree.clear()

        for name, agent_cfg in self.config.agents.items():
            node = tree.root.add(f"[bold]{name}[/]", expand=True)
            if agent_cfg.depends_on:
                for dep in agent_cfg.depends_on:
                    node.add_leaf(f"[dim]depends on → {dep}[/]")
            else:
                node.add_leaf("[dim]no dependencies[/]")

    def _poll_agent_logs(self) -> None:
        if not self.daemon:
            return

        for name in self._agent_names:
            agent = self.daemon.process_manager.get(name)
            if not agent:
                continue

            log_widget = self.query_one(f"#log-{name}", RichLog)

            while True:
                try:
                    output = agent._output_queue.get_nowait()
                    log_widget.write(output)
                except asyncio.QueueEmpty:
                    break

    async def _update_blackboard_display(self) -> None:
        if not self.daemon:
            return

        data = await self.daemon.blackboard.read_all()
        display = self.query_one("#blackboard-section", Static)

        if not data:
            display.update("Blackboard: (empty)")
        else:
            items = [f"[cyan]{k}[/]: {v}" for k, v in list(data.items())[:5]]
            if len(data) > 5:
                items.append(f"... and {len(data) - 5} more")
            display.update("Blackboard: " + " | ".join(items))

    def _update_status(self, text: str) -> None:
        status_bar = self.query_one("#status-bar", StatusBar)
        status_bar.status_text = text

    async def on_input_submitted(self, event: Input.Submitted) -> None:
        command = event.value.strip()
        if not command:
            return

        event.input.clear()

        for name in self._agent_names:
            log_widget = self.query_one(f"#log-{name}", RichLog)
            log_widget.write(f"[bold magenta]>>> {command}[/]")

        if self.daemon:
            self._update_status(f"Sending: {command[:50]}...")
            self.run_worker(self._send_command(command))

    @work(exclusive=True)
    async def _send_command(self, command: str) -> None:
        if not self.daemon:
            return

        results = await self.daemon.broadcast_to_all(command)

        for name, response in results.items():
            log_widget = self.query_one(f"#log-{name}", RichLog)
            preview = response[:200] + "..." if len(response) > 200 else response
            log_widget.write(f"[dim]{preview}[/]")

        self._update_status(f"Mesh active - {len(results)} responses")

    def action_refresh(self) -> None:
        self._build_dependency_tree()
        self.notify("Refreshed")

    def action_toggle_blackboard(self) -> None:
        section = self.query_one("#blackboard-section")
        section.toggle_class("hidden")

    def action_focus_input(self) -> None:
        self.query_one("#command-input").focus()


async def run_tui(config: RepowireConfig) -> None:
    from repowire.daemon import RepowireDaemon

    daemon = RepowireDaemon(config)
    app = RepowireTUI(config, daemon=daemon)

    try:
        await app.run_async()
    finally:
        await daemon.shutdown()
