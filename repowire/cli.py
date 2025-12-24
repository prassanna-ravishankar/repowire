from __future__ import annotations

import asyncio
import json
from pathlib import Path

import click
from rich.console import Console

from repowire import __version__

console = Console()


@click.group()
@click.version_option(version=__version__)
def main() -> None:
    """Repowire - Lateral Mesh for Coding Agents"""
    pass


@main.command()
@click.option(
    "--port",
    default=9876,
    help="Port for HTTP transport (default: 9876)",
)
@click.option(
    "--stdio",
    is_flag=True,
    help="Use stdio transport instead of HTTP",
)
def mesh(port: int, stdio: bool) -> None:
    """Start the Repowire mesh MCP server."""
    from repowire.mesh.server import run_mesh_server

    console.print(f"[bold blue]Repowire Mesh v{__version__}[/]")

    if stdio:
        console.print("Running in stdio mode...")
    else:
        console.print(f"Listening on [cyan]http://localhost:{port}[/]")
        console.print(f"MCP endpoint: [cyan]http://localhost:{port}/mcp[/]")
        console.print("\nWaiting for peers to connect...")

    run_mesh_server(port=port, stdio=stdio)


@main.command()
def status() -> None:
    """Show mesh status, peers, and shared state."""
    from repowire.mesh.state import SharedState

    state = SharedState()

    console.print("[bold blue]Repowire Mesh Status[/]")
    console.print("=" * 40)

    console.print(f"\nState file: [dim]{state._persist_path}[/]")

    async def get_state() -> dict:
        return await state.read()

    state_data = asyncio.run(get_state())

    if state_data:
        console.print(f"\n[bold]Shared State[/] ({len(state_data)} keys):")
        for key, value in state_data.items():
            val_str = json.dumps(value) if isinstance(value, (dict, list)) else str(value)
            if len(val_str) > 60:
                val_str = val_str[:60] + "..."
            console.print(f"  [cyan]{key}[/]: {val_str}")
    else:
        console.print("\n[dim]No shared state.[/]")

    console.print("\n[yellow]Note:[/] Peer list is only available when mesh server is running.")
    console.print("Start the mesh with: [cyan]repowire mesh[/]")


@main.group()
def auth() -> None:
    """Authentication commands."""
    pass


@auth.command(name="happy")
@click.option("--token", help="Import existing Happy token")
@click.option("--secret", help="Import existing Happy secret (base64url)")
def auth_happy(token: str | None, secret: str | None) -> None:
    """Authenticate with Happy Cloud."""
    from repowire.auth.happy import HappyCredentials, save_credentials

    if token and secret:
        creds = HappyCredentials(token=token, secret=secret)
        save_credentials(creds)
        console.print("[bold green]Happy credentials saved![/]")
        console.print("Stored in ~/.repowire/credentials.json")
    else:
        console.print("[yellow]To authenticate with Happy:[/]")
        console.print("1. Find your Happy credentials in ~/.happy/credentials.json")
        console.print("2. Run: repowire auth happy --token <token> --secret <secret>")
        console.print("\nAlternatively, copy token and secret from Happy mobile app.")


if __name__ == "__main__":
    main()
