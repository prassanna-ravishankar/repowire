from __future__ import annotations

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
def mesh() -> None:
    """Start the Repowire mesh MCP server."""
    import asyncio

    from repowire.mesh.server import run_mcp_server

    asyncio.run(run_mcp_server())


@main.group()
def auth() -> None:
    """Authentication commands."""
    pass


@auth.command(name="happy")
@click.option("--secret", help="Your Happy backup secret key (XXXXX-XXXXX-... format)")
def auth_happy(secret: str | None) -> None:
    """Authenticate with Happy Cloud.

    Requires your Happy backup secret key (the one Happy asked you to save).
    This is NOT the machineKey from ~/.happy/access.key.
    """
    import asyncio

    from repowire.auth.happy import (
        HappyCredentials,
        decode_secret,
        get_token,
        normalize_secret_key,
        save_credentials,
    )

    if secret:
        try:
            # Normalize the secret (handles both base64url and backup format)
            normalized_secret = normalize_secret_key(secret)

            # Get a token from Happy Cloud
            console.print("Authenticating with Happy Cloud...")
            secret_bytes = decode_secret(normalized_secret)
            token = asyncio.run(get_token(secret_bytes))

            creds = HappyCredentials(token=token, secret=normalized_secret)
            save_credentials(creds)
            console.print("[bold green]Happy credentials saved![/]")
            console.print("Stored in ~/.repowire/credentials.json")
        except ValueError as e:
            console.print(f"[red]Error:[/] {e}")
            console.print("")
            console.print("[yellow]Make sure you're using your Happy backup secret key.[/]")
            console.print("This is the key Happy asked you to save when you signed up.")
            console.print("Format: XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-...")
        except Exception as e:
            console.print(f"[red]Authentication failed:[/] {e}")
    else:
        console.print("[yellow]To authenticate with Happy:[/]")
        console.print("")
        console.print("  [cyan]repowire auth happy --secret YOUR-BACKUP-KEY[/]")
        console.print("")
        console.print("Your backup key is the secret Happy asked you to save when you signed up.")
        console.print("Format: XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-...")


if __name__ == "__main__":
    main()
