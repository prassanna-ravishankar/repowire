from __future__ import annotations

import asyncio
from pathlib import Path

import click
from rich.console import Console
from rich.table import Table

from repowire import __version__
from repowire.config import RepowireConfig


console = Console()


@click.group()
@click.version_option(version=__version__)
def main() -> None:
    pass


@main.command()
@click.option(
    "-c",
    "--config",
    type=click.Path(exists=True, path_type=Path),
    help="Path to repowire.yaml config file",
)
@click.option(
    "--tui/--no-tui",
    default=True,
    help="Launch with TUI (War Room) interface",
)
def up(config: Path | None, tui: bool) -> None:
    try:
        if config:
            cfg = RepowireConfig.from_yaml(config)
        else:
            cfg = RepowireConfig.find_and_load()
    except FileNotFoundError as e:
        console.print(f"[bold red]Error:[/] {e}")
        raise SystemExit(1)

    console.print(f"[bold blue]Repowire v{__version__}[/]")
    console.print(f"Starting mesh: [cyan]{cfg.name}[/]")

    if tui:
        from repowire.tui import run_tui

        asyncio.run(run_tui(cfg))
    else:
        from repowire.daemon import run_daemon

        asyncio.run(run_daemon(config))


@main.command()
@click.option(
    "-c",
    "--config",
    type=click.Path(exists=True, path_type=Path),
    help="Path to repowire.yaml config file",
)
def status(config: Path | None) -> None:
    try:
        if config:
            cfg = RepowireConfig.from_yaml(config)
        else:
            cfg = RepowireConfig.find_and_load()
    except FileNotFoundError as e:
        console.print(f"[bold red]Error:[/] {e}")
        raise SystemExit(1)

    table = Table(title=f"Repowire: {cfg.name}")
    table.add_column("Agent", style="cyan")
    table.add_column("Path", style="dim")
    table.add_column("Port", style="green")
    table.add_column("Model", style="yellow")
    table.add_column("Dependencies", style="magenta")

    for name, agent in cfg.agents.items():
        port = cfg.get_agent_port(name)
        deps = ", ".join(agent.depends_on) if agent.depends_on else "-"
        table.add_row(name, agent.path, str(port), agent.model, deps)

    console.print(table)


@main.command()
def init() -> None:
    config_path = Path.cwd() / "repowire.yaml"

    if config_path.exists():
        console.print("[bold yellow]Warning:[/] repowire.yaml already exists")
        if not click.confirm("Overwrite?"):
            return

    example_config = """name: "My Project"

agents:
  backend:
    path: "./backend"
    model: "claude-sonnet-4-20250514"
    color: "blue"
    capabilities:
      - "api"
      - "database"
    description: "Backend API service"

  frontend:
    path: "./frontend"
    model: "claude-sonnet-4-20250514"
    color: "green"
    depends_on:
      - "backend"
    description: "Frontend application"

settings:
  port_range_start: 3001
  blackboard_file: ".repowire/blackboard.json"
  log_dir: ".repowire/logs"
  git_branch_warnings: true
"""

    config_path.write_text(example_config)
    console.print(f"[bold green]Created:[/] {config_path}")
    console.print("Edit this file to configure your agents, then run [cyan]repowire up[/]")


if __name__ == "__main__":
    main()
