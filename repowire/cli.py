from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import TYPE_CHECKING

import click
from rich.console import Console
from rich.table import Table

from repowire import __version__

if TYPE_CHECKING:
    pass

console = Console()

# Default HTTP daemon settings
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8377


@click.group()
@click.version_option(version=__version__)
def main() -> None:
    """Repowire - Mesh network for Claude Code sessions."""
    pass


# =============================================================================
# serve command - starts HTTP daemon (replaces daemon start)
# =============================================================================


@main.command()
@click.option("--host", default=DEFAULT_HOST, help="Bind address")
@click.option("--port", default=DEFAULT_PORT, type=int, help="Port")
@click.option("--relay", is_flag=True, help="Enable relay mode")
@click.option("--backend", type=click.Choice(["claudemux", "opencode"]), help="Backend override")
def serve(host: str, port: int, relay: bool, backend: str | None) -> None:
    """Start the repowire HTTP daemon."""
    import uvicorn

    from repowire.daemon.app import create_app

    app = create_app(backend_override=backend, relay_mode=relay)
    console.print(f"[cyan]Starting Repowire daemon on {host}:{port}...[/]")
    if backend:
        console.print(f"[dim]Backend: {backend}[/]")
    if relay:
        console.print("[dim]Relay mode enabled[/]")
    uvicorn.run(app, host=host, port=port)


# =============================================================================
# setup command - one-time setup with backend option
# =============================================================================


@main.command()
@click.option("--dev", is_flag=True, help="Use dev mode (uv run from current directory)")
@click.option(
    "--backend", type=click.Choice(["claudemux", "opencode"]), help="Backend to configure"
)
def setup(dev: bool, backend: str | None) -> None:
    """One-time setup: install hooks/plugins and configure MCP server."""
    import subprocess

    from repowire.config.models import load_config

    config = load_config()

    # Determine backend: explicit > config > detect > prompt
    if not backend:
        backend = config.daemon.backend

    if not backend:
        # Auto-detect based on what's available
        backend = _detect_backend()

    if not backend:
        # Prompt user
        backend = click.prompt(
            "Which backend would you like to use?",
            type=click.Choice(["claudemux", "opencode"]),
            default="claudemux",
        )

    # Save backend choice to config
    config.daemon.backend = backend
    config.save()
    console.print(f"[green]✓[/] Backend set to: {backend}")

    if backend == "claudemux":
        _setup_claudemux(dev=dev)
    elif backend == "opencode":
        _setup_opencode(dev=dev)

    console.print("")
    console.print(f"[green]Setup complete![/] Restart your IDE to use Repowire.")


def _detect_backend() -> str | None:
    """Detect which backend to use based on available tools."""
    import shutil

    # Check if claude command exists
    if shutil.which("claude"):
        return "claudemux"
    # Check if opencode config exists
    opencode_config = Path.home() / ".config" / "opencode"
    if opencode_config.exists():
        return "opencode"
    return None


def _setup_claudemux(dev: bool = False) -> None:
    """Setup for claudemux backend."""
    import subprocess

    from repowire.hooks.installer import install_hooks

    install_hooks(dev=dev)
    console.print("[green]✓[/] Claude Code hooks installed")

    # Remove existing repowire MCP server if present
    subprocess.run(["claude", "mcp", "remove", "repowire"], capture_output=True)

    if dev:
        project_dir = str(Path(__file__).parent.parent)
        cmd = [
            "claude",
            "mcp",
            "add",
            "-s",
            "user",
            "repowire",
            "--",
            "uv",
            "run",
            "--directory",
            project_dir,
            "repowire",
            "mcp",
        ]
    else:
        cmd = ["claude", "mcp", "add", "-s", "user", "repowire", "--", "uvx", "repowire", "mcp"]

    subprocess.run(cmd, check=True)
    console.print("[green]✓[/] MCP server added to Claude")


def _setup_opencode(dev: bool = False, global_install: bool = False) -> None:
    """Setup for opencode backend."""
    from repowire.backends import get_backend

    try:
        backend = get_backend("opencode")
        backend.install(dev=dev, global_install=global_install)
        console.print("[green]✓[/] OpenCode plugin installed")
    except Exception as e:
        console.print(f"[red]Failed to install OpenCode plugin: {e}[/]")


@main.command()
def mcp() -> None:
    """Start the MCP server (for Claude Code integration)."""
    from repowire.mcp.server import run_mcp_server

    asyncio.run(run_mcp_server())


# =============================================================================
# claudemux command group - manages Claude Code hooks
# =============================================================================


@main.group()
def claudemux() -> None:
    """Manage Claude Code hooks (claudemux backend)."""
    pass


@claudemux.command(name="install")
@click.option("--dev", is_flag=True, help="Use dev mode")
def claudemux_install(dev: bool) -> None:
    """Install Repowire hooks into Claude Code."""
    from repowire.hooks.installer import install_hooks

    try:
        install_hooks(dev=dev)
        console.print("[green]Hooks installed successfully![/]")
        console.print("Claude Code will now notify Repowire when responses complete.")
    except Exception as e:
        console.print(f"[red]Failed to install hooks: {e}[/]")


@claudemux.command(name="uninstall")
def claudemux_uninstall() -> None:
    """Remove Repowire hooks from Claude Code."""
    from repowire.hooks.installer import uninstall_hooks

    try:
        uninstall_hooks()
        console.print("[green]Hooks uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall hooks: {e}[/]")


@claudemux.command(name="status")
def claudemux_status() -> None:
    """Check if hooks are installed."""
    from repowire.hooks.installer import check_hooks_installed

    if check_hooks_installed():
        console.print("[green]Hooks are installed.[/]")
    else:
        console.print("[yellow]Hooks are not installed.[/]")
        console.print("Run 'repowire claudemux install' to set up.")


# =============================================================================
# opencode command group - manages OpenCode plugin
# =============================================================================


@main.group()
def opencode() -> None:
    """Manage OpenCode plugin (opencode backend)."""
    pass


@opencode.command(name="install")
@click.option("--global", "global_install", is_flag=True, help="Install globally")
@click.option("--dev", is_flag=True, help="Use dev mode")
def opencode_install(global_install: bool, dev: bool) -> None:
    """Install Repowire plugin for OpenCode."""
    from repowire.backends import get_backend

    try:
        backend = get_backend("opencode")
        backend.install(dev=dev, global_install=global_install)
        scope = "globally" if global_install else "for current project"
        console.print(f"[green]OpenCode plugin installed {scope}![/]")
    except NotImplementedError:
        console.print("[yellow]OpenCode backend installer not yet implemented.[/]")
    except Exception as e:
        console.print(f"[red]Failed to install plugin: {e}[/]")


@opencode.command(name="uninstall")
@click.option("--global", "global_install", is_flag=True, help="Uninstall globally")
def opencode_uninstall(global_install: bool) -> None:
    """Remove Repowire plugin from OpenCode."""
    from repowire.backends import get_backend

    try:
        backend = get_backend("opencode")
        backend.uninstall(global_install=global_install)
        scope = "globally" if global_install else "for current project"
        console.print(f"[green]OpenCode plugin uninstalled {scope}.[/]")
    except NotImplementedError:
        console.print("[yellow]OpenCode backend uninstaller not yet implemented.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall plugin: {e}[/]")


@opencode.command(name="status")
def opencode_status() -> None:
    """Check if OpenCode plugin is installed."""
    from repowire.backends import get_backend

    try:
        backend = get_backend("opencode")
        if backend.check_installed():
            console.print("[green]OpenCode plugin is installed.[/]")
        else:
            console.print("[yellow]OpenCode plugin is not installed.[/]")
            console.print("Run 'repowire opencode install' to set up.")
    except NotImplementedError:
        console.print("[yellow]OpenCode backend status check not yet implemented.[/]")
    except Exception as e:
        console.print(f"[red]Error checking status: {e}[/]")


@main.group()
def peer() -> None:
    """Manage peers in the mesh."""
    pass


def _get_daemon_url() -> str:
    """Get the daemon HTTP URL from config."""
    from repowire.config.models import load_config

    config = load_config()
    return f"http://{config.daemon.host}:{config.daemon.port}"


@peer.command(name="list")
def peer_list() -> None:
    """List all registered peers and their status."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/peers")
            resp.raise_for_status()
            peers = resp.json().get("peers", [])
    except httpx.ConnectError:
        # Fallback to config-based listing if daemon not running
        from repowire.config.models import load_config

        config = load_config()
        peers = [
            {"name": p.name, "status": "unknown", "tmux_session": p.tmux_session, "path": p.path}
            for p in config.peers.values()
        ]
        if not peers:
            console.print("[yellow]No peers registered.[/]")
            console.print("Use 'repowire peer register' to add peers.")
            return
        console.print("[yellow]Daemon not running - showing config-based list[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Error fetching peers: {e}[/]")
        return

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
        status = p.get("status", "unknown")
        status_color = (
            "green" if status == "online" else ("yellow" if status == "unknown" else "red")
        )
        table.add_row(
            p.get("name", "?"),
            f"[{status_color}]{status}[/]",
            p.get("tmux_session") or "-",
            p.get("path") or "-",
        )

    console.print(table)


@peer.command(name="register")
@click.argument("name")
@click.option("--tmux-session", "-t", help="Tmux session:window (e.g., '0:mywindow')")
@click.option("--opencode-url", "-u", help="OpenCode server URL")
@click.option("--path", "-p", help="Working directory (defaults to current)")
def peer_register(
    name: str, tmux_session: str | None, opencode_url: str | None, path: str | None
) -> None:
    """Register a peer for mesh communication."""
    import httpx

    from repowire.config.models import load_config

    actual_path = path or str(Path.cwd())

    # Try HTTP API first
    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(
                f"{_get_daemon_url()}/peers",
                json={
                    "name": name,
                    "path": actual_path,
                    "tmux_session": tmux_session,
                    "opencode_url": opencode_url,
                },
            )
            resp.raise_for_status()
            console.print(f"[green]Registered peer '{name}'[/]")
    except httpx.ConnectError:
        # Fallback to config if daemon not running
        config = load_config()
        config.add_peer(
            name=name,
            path=actual_path,
            tmux_session=tmux_session,
            opencode_url=opencode_url,
        )
        console.print(f"[green]Registered peer '{name}' (daemon not running, saved to config)[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to register peer: {e}[/]")
        return

    if tmux_session:
        console.print(f"  tmux session: {tmux_session}")
    if opencode_url:
        console.print(f"  opencode url: {opencode_url}")
    console.print(f"  path: {actual_path}")


@peer.command(name="unregister")
@click.argument("name")
def peer_unregister(name: str) -> None:
    """Unregister a peer from the mesh."""
    import httpx

    from repowire.config.models import load_config

    # Try HTTP API first
    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.delete(f"{_get_daemon_url()}/peers/{name}")
            if resp.status_code == 404:
                console.print(f"[red]Peer '{name}' not found[/]")
                return
            resp.raise_for_status()
            console.print(f"[green]Unregistered peer '{name}'[/]")
    except httpx.ConnectError:
        # Fallback to config if daemon not running
        config = load_config()
        if config.remove_peer(name):
            console.print(
                f"[green]Unregistered peer '{name}' (daemon not running, removed from config)[/]"
            )
        else:
            console.print(f"[red]Peer '{name}' not found[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to unregister peer: {e}[/]")


@peer.command(name="ask")
@click.argument("name")
@click.argument("query")
@click.option("--timeout", "-t", default=120, help="Timeout in seconds")
def peer_ask(name: str, query: str, timeout: int) -> None:
    """Ask a peer a question (CLI testing utility)."""
    import httpx

    try:
        with httpx.Client(timeout=float(timeout) + 5.0) as client:
            resp = client.post(
                f"{_get_daemon_url()}/query",
                json={
                    "to_peer": name,
                    "text": query,
                    "timeout": timeout,
                },
            )
            resp.raise_for_status()
            data = resp.json()

            if data.get("error"):
                console.print(f"[red]Error: {data['error']}[/]")
            else:
                console.print(f"[cyan]{name}:[/] {data.get('text', '')}")

    except httpx.ConnectError:
        console.print("[red]Error: Cannot connect to daemon. Run 'repowire serve' first.[/]")
    except httpx.TimeoutException:
        console.print(f"[red]Timeout: No response from {name}[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Error: {e}[/]")
    except RuntimeError as e:
        console.print(f"[red]Error: {e}[/]")


# =============================================================================
# hooks command group - backward compatibility alias for claudemux
# =============================================================================


@main.group()
def hooks() -> None:
    """Manage Claude Code hooks (alias for 'claudemux')."""
    pass


@hooks.command(name="install")
@click.option("--dev", is_flag=True, help="Use dev mode")
def hooks_install(dev: bool) -> None:
    """Install Repowire hooks into Claude Code."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claudemux'[/]")
    from repowire.hooks.installer import install_hooks

    try:
        install_hooks(dev=dev)
        console.print("[green]Hooks installed successfully![/]")
        console.print("Claude Code will now notify Repowire when responses complete.")
    except Exception as e:
        console.print(f"[red]Failed to install hooks: {e}[/]")


@hooks.command(name="uninstall")
def hooks_uninstall() -> None:
    """Remove Repowire hooks from Claude Code."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claudemux'[/]")
    from repowire.hooks.installer import uninstall_hooks

    try:
        uninstall_hooks()
        console.print("[green]Hooks uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall hooks: {e}[/]")


@hooks.command(name="status")
def hooks_status() -> None:
    """Check if hooks are installed."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claudemux'[/]")
    from repowire.hooks.installer import check_hooks_installed

    if check_hooks_installed():
        console.print("[green]Hooks are installed.[/]")
    else:
        console.print("[yellow]Hooks are not installed.[/]")
        console.print("Run 'repowire hooks install' to set up.")


# =============================================================================
# daemon command group - backward compatibility (deprecated, use 'serve')
# =============================================================================


@main.group(hidden=True)
def daemon() -> None:
    """Manage the Repowire daemon (deprecated - use 'repowire serve')."""
    pass


@daemon.command(name="start")
@click.option("--foreground", "-f", is_flag=True, help="Run in foreground (don't daemonize)")
def daemon_start(foreground: bool) -> None:
    """Start the Repowire daemon (deprecated - use 'repowire serve')."""
    console.print("[yellow]Deprecation warning: Use 'repowire serve' instead[/]")

    if foreground:
        # Redirect to the new serve command
        import uvicorn

        from repowire.config.models import load_config
        from repowire.daemon.app import create_app

        config = load_config()
        app = create_app()
        console.print(
            f"[cyan]Starting Repowire daemon on {config.daemon.host}:{config.daemon.port}...[/]"
        )
        uvicorn.run(app, host=config.daemon.host, port=config.daemon.port)
    else:
        import subprocess
        import sys

        from repowire.config.models import load_config

        config = load_config()
        project_dir = Path(__file__).parent.parent

        # Start as background process using serve
        subprocess.Popen(
            [sys.executable, "-m", "repowire.daemon.app"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
            cwd=str(project_dir),
        )
        console.print(f"[green]Daemon started in background on port {config.daemon.port}.[/]")


@daemon.command(name="stop")
def daemon_stop() -> None:
    """Stop the Repowire daemon."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(f"{_get_daemon_url()}/shutdown")
            resp.raise_for_status()
            console.print("[green]Daemon stopped.[/]")
    except httpx.ConnectError:
        console.print("[yellow]Daemon is not running.[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to stop daemon: {e}[/]")


@daemon.command(name="status")
def daemon_status() -> None:
    """Check if the daemon is running."""
    import httpx

    try:
        with httpx.Client(timeout=2.0) as client:
            resp = client.get(f"{_get_daemon_url()}/health")
            resp.raise_for_status()
            data = resp.json()
            console.print(f"[green]Daemon is running[/]")
            console.print(f"  Backend: {data.get('backend', 'unknown')}")
            console.print(f"  Relay: {'enabled' if data.get('relay_mode') else 'disabled'}")
    except httpx.ConnectError:
        console.print("[yellow]Daemon is not running.[/]")
    except httpx.HTTPStatusError:
        console.print("[yellow]Daemon is not responding properly.[/]")


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


@main.group(hidden=True)
def hook() -> None:
    """Internal hook handlers (called by Claude Code)."""
    pass


@hook.command(name="stop")
def hook_stop() -> None:
    """Handle Stop hook - capture response for pending queries."""
    import sys

    from repowire.hooks.stop_handler import main as stop_main

    sys.exit(stop_main())


@hook.command(name="session")
def hook_session() -> None:
    """Handle SessionStart/SessionEnd hooks - auto-register/unregister peers."""
    import sys

    from repowire.hooks.session_handler import main as session_main

    sys.exit(session_main())


if __name__ == "__main__":
    main()
