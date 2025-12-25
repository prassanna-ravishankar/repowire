from __future__ import annotations

import asyncio
import json
from pathlib import Path

import click
from rich.console import Console
from rich.table import Table

from repowire import __version__

console = Console()


@click.group()
@click.version_option(version=__version__)
def main() -> None:
    """Repowire - Mesh network for Claude Code sessions."""
    pass


@main.command()
def mcp() -> None:
    """Start the MCP server (for Claude Code integration)."""
    from repowire.mcp.server import run_mcp_server

    asyncio.run(run_mcp_server())


@main.group()
def peer() -> None:
    """Manage peers in the mesh."""
    pass


@peer.command(name="list")
def peer_list() -> None:
    """List all registered peers and their status."""
    from repowire.session.manager import TmuxSessionManager

    manager = TmuxSessionManager()
    peers = manager.list_peers()

    if not peers:
        console.print("[yellow]No peers registered.[/]")
        console.print("Use 'repowire peer register' to add peers.")
        return

    table = Table(title="Repowire Peers")
    table.add_column("Name", style="cyan")
    table.add_column("Status", style="green")
    table.add_column("Tmux Session")
    table.add_column("Path")

    for p in peers:
        status_color = "green" if p.status.value == "online" else "red"
        table.add_row(
            p.name,
            f"[{status_color}]{p.status.value}[/]",
            p.tmux_session or "-",
            p.path,
        )

    console.print(table)


@peer.command(name="register")
@click.argument("name")
@click.option("--tmux-session", "-t", required=True, help="Tmux session name")
@click.option("--path", "-p", help="Working directory (defaults to current)")
def peer_register(name: str, tmux_session: str, path: str | None) -> None:
    """Register a peer for mesh communication."""
    from repowire.config.models import load_config

    config = load_config()
    actual_path = path or str(Path.cwd())

    config.add_peer(name, tmux_session, actual_path)

    console.print(f"[green]Registered peer '{name}'[/]")
    console.print(f"  tmux session: {tmux_session}")
    console.print(f"  path: {actual_path}")


@peer.command(name="unregister")
@click.argument("name")
def peer_unregister(name: str) -> None:
    """Unregister a peer from the mesh."""
    from repowire.config.models import load_config

    config = load_config()

    if config.remove_peer(name):
        console.print(f"[green]Unregistered peer '{name}'[/]")
    else:
        console.print(f"[red]Peer '{name}' not found[/]")


@peer.command(name="ask")
@click.argument("name")
@click.argument("query")
@click.option("--timeout", "-t", default=120, help="Timeout in seconds")
def peer_ask(name: str, query: str, timeout: int) -> None:
    """Ask a peer a question (CLI testing utility)."""
    from repowire.session.manager import TmuxSessionManager

    async def do_ask() -> str:
        manager = TmuxSessionManager()
        await manager.start()
        try:
            return await manager.send_query(name, query, timeout=float(timeout))
        finally:
            await manager.stop()

    try:
        response = asyncio.run(do_ask())
        console.print(f"[cyan]{name}:[/] {response}")
    except TimeoutError:
        console.print(f"[red]Timeout: No response from {name}[/]")
    except ValueError as e:
        console.print(f"[red]Error: {e}[/]")


@main.group()
def hooks() -> None:
    """Manage Claude Code hooks."""
    pass


@hooks.command(name="install")
def hooks_install() -> None:
    """Install Repowire hooks into Claude Code."""
    from repowire.hooks.installer import install_hooks

    try:
        install_hooks()
        console.print("[green]Hooks installed successfully![/]")
        console.print("Claude Code will now notify Repowire when responses complete.")
    except Exception as e:
        console.print(f"[red]Failed to install hooks: {e}[/]")


@hooks.command(name="uninstall")
def hooks_uninstall() -> None:
    """Remove Repowire hooks from Claude Code."""
    from repowire.hooks.installer import uninstall_hooks

    try:
        uninstall_hooks()
        console.print("[green]Hooks uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall hooks: {e}[/]")


@hooks.command(name="status")
def hooks_status() -> None:
    """Check if hooks are installed."""
    from repowire.hooks.installer import check_hooks_installed

    if check_hooks_installed():
        console.print("[green]Hooks are installed.[/]")
    else:
        console.print("[yellow]Hooks are not installed.[/]")
        console.print("Run 'repowire hooks install' to set up.")


@main.group()
def daemon() -> None:
    """Manage the Repowire daemon."""
    pass


@daemon.command(name="start")
@click.option("--relay-url", help="Relay server URL")
@click.option("--api-key", help="API key for relay authentication")
def daemon_start(relay_url: str | None, api_key: str | None) -> None:
    """Start the Repowire daemon."""
    from repowire.client.daemon import run_daemon
    from repowire.config.models import load_config

    config = load_config()

    if relay_url:
        config.relay.url = relay_url
        config.relay.enabled = True
    if api_key:
        config.relay.api_key = api_key
        config.relay.enabled = True

    console.print("[cyan]Starting Repowire daemon...[/]")
    asyncio.run(run_daemon(config))


@main.group()
def relay() -> None:
    """Manage the relay server."""
    pass


@relay.command(name="start")
@click.option("--host", default="0.0.0.0", help="Host to bind to")
@click.option("--port", default=8000, help="Port to listen on")
def relay_start(host: str, port: int) -> None:
    """Start the relay server."""
    try:
        import uvicorn

        from repowire.relay.server import create_app
    except ImportError:
        console.print("[red]Relay dependencies not installed.[/]")
        console.print("Run: pip install repowire[relay]")
        return

    console.print(f"[cyan]Starting relay server on {host}:{port}...[/]")
    uvicorn.run(create_app(), host=host, port=port)


@relay.command(name="generate-key")
@click.option("--user-id", default="default", help="User ID for the key")
@click.option("--name", default="default", help="Key name/description")
def relay_generate_key(user_id: str, name: str) -> None:
    """Generate an API key for relay authentication."""
    from repowire.relay.auth import generate_api_key

    api_key = generate_api_key(user_id, name)
    console.print(f"[green]Generated API key:[/]")
    console.print(f"  {api_key.key}")
    console.print("")
    console.print("[yellow]Save this key - it won't be shown again![/]")


@main.group()
def config() -> None:
    """Manage Repowire configuration."""
    pass


@config.command(name="show")
def config_show() -> None:
    """Show current configuration."""
    from repowire.config.models import load_config

    cfg = load_config()
    data = cfg.model_dump()

    console.print_json(json.dumps(data, indent=2, default=str))


@config.command(name="path")
def config_path() -> None:
    """Show configuration file path."""
    from repowire.config.models import Config

    console.print(str(Config.get_config_path()))


if __name__ == "__main__":
    main()
