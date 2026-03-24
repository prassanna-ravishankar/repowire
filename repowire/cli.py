from __future__ import annotations

import asyncio
import json
from pathlib import Path

import click
from rich.console import Console
from rich.table import Table

from repowire import __version__
from repowire.config.models import CACHE_DIR, DEFAULT_QUERY_TIMEOUT

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
def serve(host: str, port: int, relay: bool) -> None:
    """Start the repowire HTTP daemon."""
    import uvicorn

    from repowire.config.models import load_config
    from repowire.daemon.app import create_app

    config = load_config()
    if relay:
        config.relay.enabled = True

    if config.relay.enabled:
        config.relay.ensure_api_key()
        config.save()

    app = create_app(config=config)
    console.print(f"[cyan]Starting Repowire daemon on {host}:{port}...[/]")
    if config.relay.dashboard_url:
        console.print(f"[green]Dashboard:[/] {config.relay.dashboard_url}")
    uvicorn.run(app, host=host, port=port, ws_ping_interval=None, ws_ping_timeout=None)


# =============================================================================
# setup command - auto-detects installed agent types
# =============================================================================


@main.command()
@click.option("--no-service", is_flag=True, help="Skip daemon service installation")
@click.option("--relay", is_flag=True, help="Enable hosted relay via repowire.io")
def setup(no_service: bool, relay: bool) -> None:
    """One-time setup: install hooks/plugins, MCP server, and daemon service."""
    import shutil

    _cleanup_legacy_artifacts()

    agents_setup: list[str] = []

    # Detect and set up Claude Code if claude CLI available
    if shutil.which("claude"):
        _setup_claude_code()
        agents_setup.append("claude-code")

    # Detect and set up OpenCode if opencode CLI or config exists
    if shutil.which("opencode") or (Path.home() / ".config" / "opencode").exists():
        _setup_opencode()
        agents_setup.append("opencode")

    if not agents_setup:
        console.print("[yellow]No agent types detected.[/]")
        console.print("Install 'claude' (Claude Code) or 'opencode' first.")
        return

    console.print(f"[green]✓[/] Configured agents: {', '.join(agents_setup)}")

    # Enable relay if requested
    if relay:
        from repowire.config.models import load_config

        config = load_config()
        config.relay.enabled = True
        config.relay.ensure_api_key()
        config.save()
        console.print("[green]✓[/] Relay enabled")
        console.print(f"  Dashboard: {config.relay.dashboard_url}")

    # Install daemon as system service
    if not no_service:
        from repowire.service.installer import get_platform, install_service

        platform = get_platform()
        if platform != "unsupported":
            success, message = install_service()
            if success:
                console.print(f"[green]✓[/] Daemon service installed ({platform})")
            else:
                console.print(f"[yellow]![/] Service install failed: {message}")
                console.print("    You can run 'repowire serve' manually instead.")
        else:
            console.print("[dim]Skipping service install (unsupported platform)[/]")

    console.print("")
    console.print("[green]Setup complete![/]")
    if no_service:
        console.print("Run 'repowire serve' to start the daemon manually.")
    else:
        console.print("Daemon is running. Restart your IDE to use Repowire.")
    console.print("")
    console.print("[dim]To allow MCP spawn_peer, add to ~/.repowire/config.yaml:[/]")
    console.print("[dim]  daemon:[/]")
    console.print("[dim]    spawn:[/]")
    console.print("[dim]      allowed_commands: [claude]         # exact match[/]")
    console.print("[dim]      allowed_paths: [~/git, ~/projects] # path must be under one[/]")
    console.print("[dim]  (both lists must be set; spawn is disabled by default)[/]")


@main.command(name="build-ui")
def build_ui() -> None:
    """Build the web dashboard."""
    import subprocess
    import sys

    web_dir = Path(__file__).parent.parent / "web"
    if not web_dir.exists():
        console.print("[red]Error: 'web' directory not found.[/]")
        sys.exit(1)

    console.print("[cyan]Building web dashboard...[/]")

    # npm install
    console.print("[dim]Running npm install...[/]")
    try:
        subprocess.run(["npm", "install"], cwd=web_dir, check=True)
    except FileNotFoundError:
        console.print(
            "[red]Failed to run npm install: npm command not found. "
            "Please ensure Node.js and npm are installed and in your PATH.[/]"
        )
        sys.exit(1)
    except subprocess.CalledProcessError as e:
        console.print(f"[red]Failed to run npm install: {e}[/]")
        sys.exit(1)

    # npm run build
    console.print("[dim]Running npm run build...[/]")
    try:
        subprocess.run(["npm", "run", "build"], cwd=web_dir, check=True)
        console.print("[green]✓ Web dashboard built successfully![/]")
        console.print("Run 'repowire serve' to view it at http://localhost:8377/dashboard")
    except subprocess.CalledProcessError as e:
        console.print(f"[red]Failed to build web dashboard: {e}[/]")
        sys.exit(1)


@main.command()
def uninstall() -> None:
    """Remove all repowire components: hooks, MCP server, and daemon service."""
    from repowire.service.installer import get_service_status, uninstall_service

    console.print("[cyan]Uninstalling repowire...[/]")

    # Uninstall daemon service
    status = get_service_status()
    if status.get("installed"):
        success, message = uninstall_service()
        if success:
            console.print("[green]✓[/] Daemon service removed")
        else:
            console.print(f"[yellow]![/] {message}")
    else:
        console.print("[dim]Daemon service not installed[/]")

    # Uninstall all agent components (try both)
    _uninstall_claude_code()
    _uninstall_opencode()

    console.print("")
    console.print("[green]Uninstall complete![/]")


def _uninstall_claude_code() -> None:
    """Uninstall Claude Code components."""
    import subprocess

    from repowire.installers.claude_code import uninstall_hooks

    # Remove hooks
    try:
        if uninstall_hooks():
            console.print("[green]✓[/] Claude Code hooks removed")
        else:
            console.print("[dim]Claude Code hooks not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove hooks: {e}")

    # Remove MCP server
    result = subprocess.run(
        ["claude", "mcp", "remove", "repowire"],
        capture_output=True,
    )
    if result.returncode == 0:
        console.print("[green]✓[/] MCP server removed from Claude")
    else:
        console.print("[dim]MCP server was not registered[/]")


def _uninstall_opencode() -> None:
    """Uninstall OpenCode components."""
    from repowire.installers.opencode import uninstall_plugin

    try:
        if uninstall_plugin():
            console.print("[green]✓[/] OpenCode plugin removed")
        else:
            console.print("[dim]OpenCode plugin not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove OpenCode plugin: {e}")


@main.command()
def status() -> None:
    """Show repowire installation and daemon status."""
    import shutil

    import httpx

    from repowire.installers.claude_code import check_hooks_installed
    from repowire.service.installer import get_platform, get_service_status

    console.print("[cyan]Mode:[/] unified WebSocket")
    console.print(f"[cyan]Platform:[/] {get_platform()}")
    console.print("")

    # Check available agent types
    console.print("[cyan]Agent Types:[/]")
    if shutil.which("claude"):
        if check_hooks_installed():
            console.print("  [green]✓[/] claude-code (hooks installed)")
        else:
            console.print("  [yellow]✗[/] claude-code (hooks not installed)")
    else:
        console.print("  [dim]✗[/] claude-code (claude CLI not found)")

    if shutil.which("opencode") or (Path.home() / ".config" / "opencode").exists():
        console.print("  [green]✓[/] opencode (available)")
    else:
        console.print("  [dim]✗[/] opencode (not detected)")
    console.print("")

    # Check daemon service
    svc_status = get_service_status()
    if svc_status.get("installed"):
        if svc_status.get("running"):
            pid = svc_status.get("pid")
            pid_str = f" (PID {pid})" if pid else ""
            console.print(f"[green]✓[/] Daemon service running{pid_str}")
        else:
            console.print("[yellow]✗[/] Daemon service installed but not running")
    else:
        console.print("[dim]✗[/] Daemon service not installed")

    # Check daemon HTTP endpoint
    try:
        with httpx.Client(timeout=2.0) as client:
            resp = client.get(f"{_get_daemon_url()}/health")
            resp.raise_for_status()
            console.print(f"[green]✓[/] Daemon responding at {_get_daemon_url()}")
    except httpx.ConnectError:
        console.print(f"[yellow]✗[/] Daemon not responding at {_get_daemon_url()}")
    except Exception:
        console.print(f"[yellow]✗[/] Daemon error at {_get_daemon_url()}")

    # Show peers
    try:
        with httpx.Client(timeout=2.0) as client:
            resp = client.get(f"{_get_daemon_url()}/peers")
            resp.raise_for_status()
            peers = resp.json().get("peers", [])
            online = [p for p in peers if p.get("status") == "online"]
            console.print(f"[cyan]Peers:[/] {len(online)} online, {len(peers)} total")
    except Exception:
        pass


def _cleanup_legacy_artifacts() -> None:
    """Remove file artifacts from pre-lazy-repair versions.

    Prior versions wrote .pid, .sid, .name, .uname files per tmux pane,
    plus correlation/ and response/ directories. None of these are used
    anymore — the daemon tracks all state in-memory via WebSocket.
    """
    import shutil

    # Remove per-pane hook files (.pid, .sid, .name, .uname)
    hooks_dir = CACHE_DIR / "hooks"
    if hooks_dir.is_dir():
        count = sum(1 for f in hooks_dir.iterdir() if f.is_file())
        if count:
            shutil.rmtree(hooks_dir)
            console.print(f"[green]\u2713[/] Cleaned {count} legacy hook files")

    # Remove correlation and response directories
    for dirname in ("correlations", "responses"):
        d = CACHE_DIR / dirname
        if d.is_dir():
            shutil.rmtree(d)
            console.print(f"[green]\u2713[/] Removed legacy {dirname}/ directory")


def _setup_claude_code() -> None:
    """Setup for Claude Code agent type."""
    import subprocess

    from repowire.installers.claude_code import install_channel, install_hooks

    # Try channel transport first (Claude Code v2.1.80+ with bun)
    channel_ok, channel_msg = install_channel()
    if channel_ok:
        install_hooks(channel_mode=True)  # minimal Stop hook for dashboard
        console.print(f"[green]✓[/] {channel_msg}")
    else:
        install_hooks()  # full hooks for legacy transport
        console.print(f"[yellow]![/] {channel_msg}")
        console.print("[green]✓[/] Claude Code hooks installed (legacy transport)")

    # Remove existing repowire MCP server if present
    subprocess.run(["claude", "mcp", "remove", "repowire"], capture_output=True)

    cmd = ["claude", "mcp", "add", "-s", "user", "repowire", "--", "repowire", "mcp"]
    subprocess.run(cmd, check=True)
    console.print("[green]✓[/] MCP server added to Claude")


def _setup_opencode() -> None:
    """Setup for OpenCode agent type."""
    from repowire.installers.opencode import install_plugin

    try:
        install_plugin()
        console.print("[green]✓[/] OpenCode plugin installed")
    except Exception as e:
        console.print(f"[red]Failed to install OpenCode plugin: {e}[/]")


@main.command()
def mcp() -> None:
    """Start the MCP server (for Claude Code integration)."""
    from repowire.mcp.server import run_mcp_server

    asyncio.run(run_mcp_server())


# =============================================================================
# claude command group - manages Claude Code hooks
# =============================================================================


@main.group(hidden=True)
def claude() -> None:
    """Manage Claude Code hooks."""
    pass


@claude.command(name="install")
def claude_install() -> None:
    """Install Repowire hooks into Claude Code."""
    from repowire.installers.claude_code import install_hooks

    try:
        install_hooks()
        console.print("[green]Hooks installed successfully![/]")
        console.print("Claude Code will now notify Repowire when responses complete.")
    except Exception as e:
        console.print(f"[red]Failed to install hooks: {e}[/]")


@claude.command(name="uninstall")
def claude_uninstall() -> None:
    """Remove Repowire hooks from Claude Code."""
    from repowire.installers.claude_code import uninstall_hooks

    try:
        uninstall_hooks()
        console.print("[green]Hooks uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall hooks: {e}[/]")


@claude.command(name="status")
def claude_status() -> None:
    """Check if hooks are installed."""
    from repowire.installers.claude_code import check_hooks_installed

    if check_hooks_installed():
        console.print("[green]Hooks are installed.[/]")
    else:
        console.print("[yellow]Hooks are not installed.[/]")
        console.print("Run 'repowire claude install' to set up.")


# =============================================================================
# opencode command group - manages OpenCode plugin
# =============================================================================


@main.group(hidden=True)
def opencode() -> None:
    """Manage OpenCode plugin."""
    pass


@opencode.command(name="install")
@click.option("--global", "global_install", is_flag=True, help="Install globally")
def opencode_install(global_install: bool) -> None:
    """Install Repowire plugin for OpenCode."""
    from repowire.installers.opencode import install_plugin

    try:
        install_plugin(global_install=global_install)
        scope = "globally" if global_install else "for current project"
        console.print(f"[green]OpenCode plugin installed {scope}![/]")
    except Exception as e:
        console.print(f"[red]Failed to install plugin: {e}[/]")


@opencode.command(name="uninstall")
@click.option("--global", "global_install", is_flag=True, help="Uninstall globally")
def opencode_uninstall(global_install: bool) -> None:
    """Remove Repowire plugin from OpenCode."""
    from repowire.installers.opencode import uninstall_plugin

    try:
        uninstall_plugin(global_install=global_install)
        scope = "globally" if global_install else "for current project"
        console.print(f"[green]OpenCode plugin uninstalled {scope}.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall plugin: {e}[/]")


@opencode.command(name="status")
def opencode_status() -> None:
    """Check if OpenCode plugin is installed."""
    from repowire.installers.opencode import check_plugin_installed

    try:
        if check_plugin_installed():
            console.print("[green]OpenCode plugin is installed.[/]")
        else:
            console.print("[yellow]OpenCode plugin is not installed.[/]")
            console.print("Run 'repowire opencode install' to set up.")
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
    table.add_column("Circle", style="magenta")
    table.add_column("Tmux Session")
    table.add_column("Path")

    for p in peers:
        status = p.get("status", "unknown")
        if status == "online":
            status_color = "green"
        elif status == "unknown":
            status_color = "yellow"
        else:
            status_color = "red"
        table.add_row(
            p.get("name", "?"),
            f"[{status_color}]{status}[/]",
            p.get("circle") or "global",
            p.get("tmux_session") or "-",
            p.get("path") or "-",
        )

    console.print(table)


@peer.command(name="new")
@click.argument("path", type=click.Path(exists=True), default=".")
@click.option(
    "--backend", "-b", type=click.Choice(["claude-code", "opencode"]), default="claude-code"
)
@click.option("--command", "-c", "cmd", help="Command to run (default: claude or opencode)")
@click.option("--circle", help="Circle (defaults to 'default')")
def peer_new(path: str, backend: str, cmd: str | None, circle: str | None) -> None:  # noqa: ARG001
    """Spawn a new peer in a tmux window.

    Examples:

        repowire peer new ~/git/myproject

        repowire peer new . --command="claude --dangerously-skip-permissions"

        repowire peer new ~/git/api --backend=opencode --circle=backend
    """
    from repowire.config.models import AgentType
    from repowire.spawn import SpawnConfig, spawn_peer

    actual_path = str(Path(path).resolve())
    actual_circle = circle or "default"
    actual_cmd = cmd or ("claude" if backend == "claude-code" else "opencode")
    backend_type = AgentType(backend)

    config = SpawnConfig(
        path=actual_path,
        circle=actual_circle,
        backend=backend_type,
        command=actual_cmd,
    )

    try:
        result = spawn_peer(config)
        console.print(
            f"[green]✓[/] Spawned [cyan]{result.display_name}[/] "
            f"in circle [magenta]{actual_circle}[/]"
        )
        console.print(f"  tmux: {result.tmux_session}")
        console.print(f"  command: {actual_cmd}")
        console.print("[dim]  (will auto-register via WebSocket)[/]")

    except ValueError as e:
        console.print(f"[red]Error: {e}[/]")
    except Exception as e:
        err_msg = str(e) if str(e) else type(e).__name__
        console.print(f"[red]Failed to spawn: {err_msg}[/]")


@peer.command(name="register")
@click.argument("name")
@click.option("--tmux-session", "-t", help="Tmux session:window (e.g., '0:mywindow')")
@click.option("--circle", "-c", help="Circle (logical subnet)")
@click.option("--path", "-p", help="Working directory (defaults to current)")
def peer_register(
    name: str,
    tmux_session: str | None,
    circle: str | None,
    path: str | None,
) -> None:
    """Register a peer for mesh communication."""
    import httpx

    actual_path = path or str(Path.cwd())

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(
                f"{_get_daemon_url()}/peers",
                json={
                    "name": name,
                    "path": actual_path,
                    "tmux_session": tmux_session,
                    "circle": circle,
                },
            )
            resp.raise_for_status()
            console.print(f"[green]Registered peer '{name}'[/]")
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        return
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to register peer: {e}[/]")
        return

    if tmux_session:
        console.print(f"  tmux session: {tmux_session}")
    if circle:
        console.print(f"  circle: {circle}")
    console.print(f"  path: {actual_path}")


@peer.command(name="unregister")
@click.argument("name")
def peer_unregister(name: str) -> None:
    """Unregister a peer from the mesh."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.delete(f"{_get_daemon_url()}/peers/{name}")
            if resp.status_code == 404:
                console.print(f"[red]Peer '{name}' not found[/]")
                return
            resp.raise_for_status()
            console.print(f"[green]Unregistered peer '{name}'[/]")
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to unregister peer: {e}[/]")


@peer.command(name="ask")
@click.argument("name")
@click.argument("query")
@click.option("--timeout", "-t", default=DEFAULT_QUERY_TIMEOUT, help="Timeout in seconds")
@click.option("--circle", "-c", default=None, help="Circle to scope peer lookup")
def peer_ask(name: str, query: str, timeout: int, circle: str | None) -> None:
    """Ask a peer a question (CLI testing utility)."""
    import httpx

    try:
        with httpx.Client(timeout=float(timeout) + 5.0) as client:
            body: dict = {
                "to_peer": name,
                "text": query,
                "timeout": timeout,
            }
            if circle:
                body["circle"] = circle
            resp = client.post(
                f"{_get_daemon_url()}/query",
                json=body,
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


@peer.command(name="prune")
@click.option("--force", "-f", is_flag=True, help="Skip confirmation")
@click.option("--dry-run", is_flag=True, help="Show what would be removed")
def peer_prune(force: bool, dry_run: bool) -> None:
    """Remove offline peers from the daemon."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/peers")
            resp.raise_for_status()
            peers = resp.json().get("peers", [])
    except httpx.RequestError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        return

    # Filter offline peers
    offline = [p for p in peers if p.get("status") == "offline"]

    if not offline:
        console.print("[green]No offline peers to prune[/]")
        return

    # Display
    console.print(f"\n[bold]Offline peers ({len(offline)}):[/]")
    for p in offline:
        console.print(f"  [dim]•[/] {p['name']}")

    if dry_run:
        console.print(f"\n[dim]Dry run - would remove {len(offline)} peer(s)[/]")
        return

    if not force and not click.confirm(f"\nRemove {len(offline)} peer(s)?"):
        return

    # Remove via daemon API
    removed = 0
    with httpx.Client(timeout=5.0) as client:
        for p in offline:
            try:
                resp = client.delete(f"{_get_daemon_url()}/peers/{p['name']}")
                if resp.status_code < 400:
                    removed += 1
                    console.print(f"[green]✓[/] Removed {p['name']}")
            except httpx.RequestError:
                console.print(f"[red]Failed to remove {p['name']}[/]")

    console.print(f"\n[bold green]Pruned {removed} peer(s)[/]")


# =============================================================================
# hooks command group - backward compatibility alias for claude
# =============================================================================


@main.group(hidden=True)
def hooks() -> None:
    """Manage Claude Code hooks (alias for 'claude')."""
    pass


@hooks.command(name="install")
def hooks_install() -> None:
    """Install Repowire hooks into Claude Code."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claude'[/]")
    from repowire.installers.claude_code import install_hooks

    try:
        install_hooks()
        console.print("[green]Hooks installed successfully![/]")
        console.print("Claude Code will now notify Repowire when responses complete.")
    except Exception as e:
        console.print(f"[red]Failed to install hooks: {e}[/]")


@hooks.command(name="uninstall")
def hooks_uninstall() -> None:
    """Remove Repowire hooks from Claude Code."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claude'[/]")
    from repowire.installers.claude_code import uninstall_hooks

    try:
        uninstall_hooks()
        console.print("[green]Hooks uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall hooks: {e}[/]")


@hooks.command(name="status")
def hooks_status() -> None:
    """Check if hooks are installed."""
    console.print("[dim]Note: 'repowire hooks' is an alias for 'repowire claude'[/]")
    from repowire.installers.claude_code import check_hooks_installed

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
            console.print("[green]Daemon is running[/]")
            console.print(f"  Relay: {'enabled' if data.get('relay_mode') else 'disabled'}")
    except httpx.ConnectError:
        console.print("[yellow]Daemon is not running.[/]")
    except httpx.HTTPStatusError:
        console.print("[yellow]Daemon is not responding properly.[/]")


@main.group(hidden=True)
def relay() -> None:
    """Manage the relay server."""
    pass


@relay.command(name="start")
@click.option("--host", default="0.0.0.0", help="Host to bind to")
@click.option("--port", default=8000, help="Port to listen on")
def relay_start(host: str, port: int) -> None:
    """Start the relay server."""
    import uvicorn

    from repowire.relay.server import create_app

    console.print(f"[cyan]Starting relay server on {host}:{port}...[/]")
    uvicorn.run(create_app(), host=host, port=port, ws_ping_interval=None, ws_ping_timeout=None)


@relay.command(name="generate-key")
@click.option("--user-id", default="default", help="User ID for the key")
def relay_generate_key(user_id: str) -> None:
    """Generate an API key for relay authentication."""
    from repowire.relay.auth import register_token

    api_key = register_token(user_id)
    console.print("[green]Generated API key:[/]")
    console.print(f"  {api_key.key}")
    console.print("")
    console.print("[yellow]Save this key - it won't be shown again![/]")


# =============================================================================
# telegram command group
# =============================================================================


@main.group(hidden=True)
def telegram() -> None:
    """Manage the Telegram bot peer."""
    pass


@telegram.command(name="start")
def telegram_start() -> None:
    """Start the Telegram bot as a repowire peer."""
    from repowire.telegram.bot import main as bot_main

    bot_main()


# =============================================================================
# service command group - system service management
# =============================================================================


@main.group(hidden=True)
def service() -> None:
    """Manage repowire daemon as a system service."""
    pass


@service.command(name="install")
def service_install() -> None:
    """Install repowire daemon as a system service (launchd/systemd)."""
    from repowire.service.installer import get_platform, install_service

    platform = get_platform()
    if platform == "unsupported":
        console.print("[red]Unsupported platform for service installation.[/]")
        console.print("Supported: macOS (launchd), Linux (systemd)")
        return

    console.print(f"[cyan]Installing repowire service ({platform})...[/]")

    success, message = install_service()

    if success:
        console.print(f"[green]{message}[/]")
        console.print("")
        console.print("The daemon will now start automatically on login.")
        console.print("Logs: ~/.repowire/daemon.log")
    else:
        console.print(f"[red]{message}[/]")


@service.command(name="uninstall")
def service_uninstall() -> None:
    """Uninstall repowire daemon system service."""
    from repowire.service.installer import uninstall_service

    success, message = uninstall_service()

    if success:
        console.print(f"[green]{message}[/]")
    else:
        console.print(f"[yellow]{message}[/]")


@service.command(name="restart")
def service_restart() -> None:
    """Restart repowire daemon system service."""
    from repowire.service.installer import restart_service

    success, message = restart_service()

    if success:
        console.print(f"[green]{message}[/]")
    else:
        console.print(f"[yellow]{message}[/]")


@service.command(name="status")
def service_status() -> None:
    """Check if repowire daemon service is installed and running."""
    from repowire.service.installer import get_platform, get_service_status

    platform = get_platform()
    status = get_service_status()

    if status.get("error"):
        console.print(f"[red]{status['error']}[/]")
        return

    console.print(f"[cyan]Platform:[/] {platform}")
    console.print(f"[cyan]Service path:[/] {status.get('path', 'N/A')}")

    if not status.get("installed"):
        console.print("[yellow]Status: Not installed[/]")
        console.print("Run 'repowire service install' to set up.")
        return

    if status.get("running"):
        pid = status.get("pid")
        pid_str = f" (PID {pid})" if pid else ""
        console.print(f"[green]Status: Running{pid_str}[/]")
    else:
        console.print("[yellow]Status: Installed but not running[/]")


@main.group(hidden=True)
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


@hook.command(name="prompt")
def hook_prompt() -> None:
    """Handle UserPromptSubmit hook - mark peer as busy."""
    import sys

    from repowire.hooks.prompt_handler import main as prompt_main

    sys.exit(prompt_main())


@hook.command(name="notification")
def hook_notification() -> None:
    """Handle Notification hook - mark peer as online on idle."""
    import sys

    from repowire.hooks.notification_handler import main as notification_main

    sys.exit(notification_main())


if __name__ == "__main__":
    main()
