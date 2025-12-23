from __future__ import annotations

import asyncio
from pathlib import Path

import click
import yaml
from rich.console import Console
from rich.table import Table

from repowire import __version__
from repowire.config import RepowireConfig


console = Console()


PROJECT_MARKERS: dict[str, tuple[list[str], str, list[str]]] = {
    "frontend": (
        ["package.json", "tsconfig.json", "vite.config.ts", "next.config.js", "angular.json"],
        "green",
        ["ui", "components"],
    ),
    "backend": (
        ["pyproject.toml", "requirements.txt", "go.mod", "Cargo.toml", "pom.xml", "build.gradle"],
        "blue",
        ["api", "database"],
    ),
    "infra": (
        ["main.tf", "terraform.tfstate", "pulumi.yaml", "serverless.yml"],
        "yellow",
        ["infrastructure", "deployment"],
    ),
    "mobile": (
        ["Podfile", "build.gradle.kts", "android/", "ios/"],
        "magenta",
        ["mobile", "app"],
    ),
    "docs": (
        ["mkdocs.yml", "docusaurus.config.js", "sphinx/"],
        "cyan",
        ["documentation"],
    ),
}


def detect_project_type(repo_path: Path) -> tuple[str, str, list[str]]:
    for proj_type, (markers, color, caps) in PROJECT_MARKERS.items():
        for marker in markers:
            if (repo_path / marker).exists():
                return proj_type, color, caps
    return "generic", "white", []


def is_git_repo(path: Path) -> bool:
    return (path / ".git").is_dir()


def scan_for_repos(root: Path, max_depth: int = 1) -> list[Path]:
    repos: list[Path] = []

    if max_depth < 0 or not root.is_dir():
        return repos

    if is_git_repo(root):
        return [root]

    try:
        for child in root.iterdir():
            if child.is_dir() and not child.name.startswith("."):
                if is_git_repo(child):
                    repos.append(child)
                elif max_depth > 0:
                    repos.extend(scan_for_repos(child, max_depth - 1))
    except PermissionError:
        pass

    return repos


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


@main.command()
@click.argument("directory", type=click.Path(exists=True, path_type=Path))
@click.option(
    "-o",
    "--output",
    type=click.Path(path_type=Path),
    default=None,
    help="Output path for repowire.yaml (default: current directory)",
)
@click.option(
    "-n",
    "--name",
    default=None,
    help="Project name (default: directory name)",
)
@click.option(
    "--depth",
    default=1,
    help="Max depth to scan for repos (default: 1)",
)
@click.option(
    "--dry-run",
    is_flag=True,
    help="Show what would be generated without writing",
)
def scan(
    directory: Path,
    output: Path | None,
    name: str | None,
    depth: int,
    dry_run: bool,
) -> None:
    directory = directory.resolve()
    repos = scan_for_repos(directory, max_depth=depth)

    if not repos:
        console.print(f"[bold yellow]No git repositories found in {directory}[/]")
        return

    repos = sorted(repos, key=lambda p: p.name)

    console.print(f"[bold blue]Found {len(repos)} repositories:[/]")

    agents: dict[str, dict] = {}
    for repo in repos:
        proj_type, color, caps = detect_project_type(repo)
        agent_name = repo.name.lower().replace("-", "_").replace(" ", "_")

        console.print(f"  [cyan]{agent_name}[/] ({proj_type}) - [{color}]{repo}[/]")

        agents[agent_name] = {
            "path": str(repo),
            "model": "claude-sonnet-4-20250514",
            "color": color,
            "capabilities": caps,
            "description": f"{proj_type.title()} project",
        }

    config_data = {
        "name": name or directory.name,
        "agents": agents,
        "settings": {
            "port_range_start": 3001,
            "blackboard_file": ".repowire/blackboard.json",
            "log_dir": ".repowire/logs",
            "git_branch_warnings": True,
        },
    }

    yaml_output = yaml.dump(config_data, default_flow_style=False, sort_keys=False)

    if dry_run:
        console.print("\n[bold]Generated config:[/]")
        console.print(yaml_output)
        return

    output_path = output or Path.cwd() / "repowire.yaml"

    if output_path.exists():
        console.print(f"[bold yellow]Warning:[/] {output_path} already exists")
        if not click.confirm("Overwrite?"):
            return

    output_path.write_text(yaml_output)
    console.print(f"\n[bold green]Created:[/] {output_path}")
    console.print("Review the config, then run [cyan]repowire up[/]")


if __name__ == "__main__":
    main()
