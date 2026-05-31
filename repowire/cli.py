from __future__ import annotations

import asyncio
import json
import secrets
from pathlib import Path
from typing import Any

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
@click.option(
    "--no-install-hooks",
    is_flag=True,
    help=(
        "Skip installing tmux lifecycle hooks on startup. Useful for smoke/test "
        "daemons under a temp HOME, since tmux hooks are server-global and would "
        "otherwise rewrite the user's live mesh hooks. Also honored via "
        "REPOWIRE_DISABLE_HOOK_INSTALL=1."
    ),
)
def serve(host: str, port: int, relay: bool, no_install_hooks: bool) -> None:
    """Start the repowire HTTP daemon."""
    import os

    import uvicorn

    from repowire.config.models import load_config
    from repowire.daemon.app import create_app

    config = load_config()
    if relay:
        config.relay.enabled = True

    if config.relay.enabled:
        config.relay.ensure_api_key()
        config.save()

    env_disable = os.environ.get("REPOWIRE_DISABLE_HOOK_INSTALL", "").strip().lower() in (
        "1",
        "true",
        "yes",
        "on",
    )
    install_hooks = not (no_install_hooks or env_disable)
    app = create_app(config=config, install_tmux_hooks=install_hooks)
    console.print(f"[cyan]Starting Repowire daemon on {host}:{port}...[/]")
    if config.relay.dashboard_url:
        console.print(f"[green]Dashboard:[/] {config.relay.dashboard_url}")
    uvicorn.run(app, host=host, port=port, ws_ping_interval=None, ws_ping_timeout=None)


# =============================================================================
# setup command - auto-detects installed agent types
# =============================================================================


def _mask_token(token: str) -> str:
    """Mask a token for display, showing only the last 4 chars."""
    if len(token) <= 8:
        return "****"
    return "****" + token[-4:]


def _is_interactive(non_interactive: bool) -> bool:
    """Check if we should run interactively."""
    import sys

    return not non_interactive and sys.stdin.isatty()


def _detect_package_manager() -> str | None:
    """Detect how repowire was installed: 'uv', 'pipx', or 'pip'."""
    import os
    import shutil
    import sys

    repowire_bin = shutil.which("repowire")
    if not repowire_bin:
        return None
    paths = [
        str(Path(repowire_bin).resolve()),
        str(Path(sys.executable).resolve()),
        os.environ.get("VIRTUAL_ENV", ""),
    ]
    normalized = [path.replace("\\", "/") for path in paths if path]
    if any("/uv/tools/repowire/" in path for path in normalized) and shutil.which("uv"):
        return "uv"
    if any("/pipx/venvs/repowire/" in path for path in normalized) and shutil.which("pipx"):
        return "pipx"
    if shutil.which("pip") or shutil.which("pip3"):
        return "pip"
    return None


_PKG_COMMANDS: dict[str, dict[str, list[str]]] = {
    "uv": {
        "upgrade": ["uv", "tool", "upgrade", "repowire"],
        "uninstall": ["uv", "tool", "uninstall", "repowire"],
    },
    "pipx": {
        "upgrade": ["pipx", "upgrade", "repowire"],
        "uninstall": ["pipx", "uninstall", "repowire"],
    },
    "pip": {
        "upgrade": ["pip", "install", "-U", "repowire"],
        "uninstall": ["pip", "uninstall", "-y", "repowire"],
    },
}


def _enabled_package_extras(config: object) -> list[str]:
    """Return optional package extras required by the current config."""
    experiments = getattr(config, "experiments", None)
    extras: list[str] = []
    if bool(getattr(experiments, "acp_broker_client", False)):
        extras.append("acp")
    return extras


def _repowire_package_spec(config: object) -> str:
    extras = _enabled_package_extras(config)
    if not extras:
        return "repowire"
    return f"repowire[{','.join(extras)}]"


def _upgrade_command(pkg_mgr: str, config: object) -> list[str]:
    """Build the package-manager command for `repowire update`.

    Package-manager upgrade commands do not all preserve extras. When an
    enabled config requires extras, reinstall the tool with the explicit
    package spec so the upgraded environment still has optional dependencies.
    """
    package_spec = _repowire_package_spec(config)
    if package_spec == "repowire":
        return _PKG_COMMANDS[pkg_mgr]["upgrade"]
    if pkg_mgr == "uv":
        return ["uv", "tool", "install", package_spec, "--upgrade", "--force"]
    if pkg_mgr == "pipx":
        return ["pipx", "install", package_spec, "--force"]
    if pkg_mgr == "pip":
        return ["pip", "install", "-U", package_spec]
    return _PKG_COMMANDS[pkg_mgr]["upgrade"]


def _prompt_bot_config(
    name: str,
    config_section: object,
    fields: list[tuple[str, str]],
) -> None:
    """Prompt user to configure a bot integration. Handles existing config display."""
    token_field = fields[0][0]  # first field is always the primary token
    has_config = bool(getattr(config_section, token_field, None))
    if has_config:
        masked = _mask_token(getattr(config_section, token_field))
        console.print(f"[dim]{name} configured (token: {masked})[/]")
        if click.confirm(f"  Reconfigure {name}?", default=False):
            has_config = False
    if not has_config and click.confirm(f"Configure {name} bot?", default=False):
        for field_name, prompt_text in fields:
            hide = "token" in field_name
            setattr(
                config_section,
                field_name,
                click.prompt(
                    f"  {prompt_text}",
                    hide_input=hide,
                ),
            )
        console.print(f"[green]✓[/] {name} configured")


def _generate_daemon_auth_token() -> str:
    """Generate a local bearer token suitable for daemon HTTP/WebSocket auth."""
    return "rw_local_" + secrets.token_urlsafe(32)


def _enable_http_mcp(config: object) -> bool:
    """Enable localhost HTTP MCP and create a bearer token if missing."""
    daemon = getattr(config, "daemon")
    changed = False
    if not getattr(daemon.mcp_http, "enabled", False):
        daemon.mcp_http.enabled = True
        changed = True
    if not daemon.auth_token:
        daemon.auth_token = _generate_daemon_auth_token()
        changed = True
    return changed


def _daemon_health_ok(host: str, port: int, *, attempts: int = 10) -> bool:
    """Briefly poll daemon health after service install/restart."""
    import time

    import httpx

    url = f"http://{host}:{port}/health"
    for _ in range(attempts):
        try:
            response = httpx.get(url, timeout=0.5)
            if response.status_code == 200:
                return True
        except httpx.HTTPError:
            pass
        time.sleep(0.2)
    return False


@main.command()
@click.option("--no-service", is_flag=True, help="Skip daemon service installation")
@click.option("--relay", is_flag=True, help="Enable hosted relay via repowire.io")
@click.option(
    "--update-checks/--no-update-checks",
    default=None,
    help=(
        "Opt status/doctor into checking for newer Repowire releases. "
        "Updates are never auto-applied."
    ),
)
@click.option(
    "--experimental-channels",
    is_flag=True,
    help="Use channel transport (experimental, requires claude.ai login and bun)",
)
@click.option(
    "--http-mcp",
    is_flag=True,
    help="Enable the experimental localhost Streamable HTTP MCP endpoint at /mcp",
)
@click.option("--non-interactive", is_flag=True, help="Skip prompts, use flags only")
def setup(
    no_service: bool,
    relay: bool,
    update_checks: bool | None,
    experimental_channels: bool,
    http_mcp: bool,
    non_interactive: bool,
) -> None:
    """One-time setup: install hooks/plugins, MCP server, and daemon service."""
    from repowire.config.models import load_config

    interactive = _is_interactive(non_interactive)
    config = load_config()

    _cleanup_legacy_artifacts()

    agents_setup = _install_detected_backends(
        config,
        interactive=interactive,
        experimental_channels=experimental_channels,
    )

    if not agents_setup:
        console.print("[yellow]No agent types detected.[/]")
        console.print("Install claude, codex, gemini, agy, opencode, or pi first.")
        if not (http_mcp or relay or update_checks is not None or interactive):
            return
    else:
        console.print(f"[green]✓[/] Configured agents: {', '.join(agents_setup)}")

    # Install tmux lifecycle hooks if tmux is available
    try:
        from repowire.hooks.tmux_lifecycle import install_hooks, is_tmux_available

        if is_tmux_available():
            installed = install_hooks(
                config.daemon.host,
                config.daemon.port,
            )
            if installed:
                console.print(f"[green]✓[/] Tmux lifecycle hooks ({len(installed)} hooks)")
    except Exception as e:
        console.print(f"[dim]Tmux hooks skipped: {e}[/]")

    # Relay: flag, existing config, or interactive prompt
    if config.relay.enabled:
        console.print("[green]✓[/] Relay enabled (existing config)")
    elif relay:
        config.relay.enabled = True
        config.relay.ensure_api_key()
        console.print("[green]✓[/] Relay enabled")
        console.print(f"  Dashboard: {config.relay.dashboard_url}")
    elif interactive:
        if click.confirm(
            "Connect to hosted relay at repowire.io for remote access?",
            default=False,
        ):
            config.relay.enabled = True
            config.relay.ensure_api_key()
            console.print("[green]✓[/] Relay enabled")
            console.print(f"  Dashboard: {config.relay.dashboard_url}")

    if config.daemon.mcp_http.enabled:
        if not config.daemon.auth_token:
            _enable_http_mcp(config)
            console.print("[green]✓[/] HTTP MCP bearer token generated")
        else:
            console.print("[green]✓[/] HTTP MCP enabled (existing config)")
    elif http_mcp:
        _enable_http_mcp(config)
        console.print("[green]✓[/] HTTP MCP enabled at http://127.0.0.1:8377/mcp")
        console.print(f"  Bearer token: {_mask_token(config.daemon.auth_token or '')}")
    elif interactive and click.confirm(
        "Enable experimental localhost HTTP MCP endpoint?",
        default=False,
    ):
        _enable_http_mcp(config)
        console.print("[green]✓[/] HTTP MCP enabled at http://127.0.0.1:8377/mcp")
        console.print(f"  Bearer token: {_mask_token(config.daemon.auth_token or '')}")

    if update_checks is not None:
        config.updates.check_enabled = update_checks
        state = "enabled" if update_checks else "disabled"
        console.print(f"[green]✓[/] Update checks {state}")
    elif interactive:
        config.updates.check_enabled = click.confirm(
            "Check for Repowire updates in status/doctor?",
            default=config.updates.check_enabled,
        )
        if config.updates.check_enabled:
            console.print("[green]✓[/] Update checks enabled")

    # Bot integrations: show existing config or prompt
    if config.telegram.bot_token:
        console.print("[green]✓[/] Telegram configured (existing config)")
    elif interactive:
        _prompt_bot_config(
            "Telegram",
            config.telegram,
            [
                ("bot_token", "Bot token"),
                ("chat_id", "Chat ID"),
            ],
        )

    if config.slack.bot_token:
        console.print("[green]✓[/] Slack configured (existing config)")
    elif interactive:
        _prompt_bot_config(
            "Slack",
            config.slack,
            [
                ("bot_token", "Bot token (xoxb-...)"),
                ("app_token", "App token (xapp-...)"),
                ("channel_id", "Channel ID (C...)"),
            ],
        )

    # Save config
    console.print("[green]✓[/] SQLite state enabled")
    console.print("  State migrations run automatically when the daemon starts.")
    config.save()

    # Install daemon as system service
    daemon_running = False
    if not no_service:
        from repowire.service.installer import get_platform, install_service

        platform = get_platform()
        if platform != "unsupported":
            success, message = install_service()
            if success:
                console.print(f"[green]✓[/] Daemon service installed ({platform})")
                daemon_running = _daemon_health_ok(config.daemon.host, config.daemon.port)
                if not daemon_running:
                    console.print(
                        "[yellow]![/] Service installed, but daemon health did not respond yet."
                    )
                    console.print("    Run 'repowire doctor' or 'repowire serve' to inspect.")
            else:
                console.print(f"[yellow]![/] Service install failed: {message}")
                console.print("    You can run 'repowire serve' manually instead.")
        else:
            console.print("[dim]Skipping service install (unsupported platform)[/]")

    console.print("")
    console.print("[green]Setup complete![/]")
    if no_service:
        console.print("Run 'repowire serve' to start the daemon manually.")
    elif daemon_running:
        console.print("Daemon is running. Restart your IDE to use Repowire.")
    else:
        console.print("Daemon service was installed, but health was not confirmed.")

    # Hints for optional features not yet configured
    hints = []
    if not config.relay.enabled:
        hints.append("Remote dashboard + multi-machine mesh: repowire setup --relay")
    if not config.telegram.bot_token:
        hints.append("Telegram bot (mobile control): repowire setup (interactive)")
    if not config.slack.bot_token:
        hints.append("Slack bot: repowire setup (interactive)")
    if not config.updates.check_enabled:
        hints.append("Update availability checks: repowire setup --update-checks")
    if hints:
        console.print("")
        console.print("[dim]Optional:[/]")
        for h in hints:
            console.print(f"[dim]  {h}[/]")


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
@click.option("--yes", "-y", is_flag=True, help="Skip confirmation prompts")
def uninstall(yes: bool) -> None:
    """Remove all repowire components: hooks, MCP server, daemon service, and optionally config."""
    import shutil
    import subprocess

    from repowire.config.models import Config
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

    # Uninstall tmux lifecycle hooks
    try:
        from repowire.hooks.tmux_lifecycle import is_tmux_available, uninstall_hooks

        if is_tmux_available():
            removed = uninstall_hooks()
            if removed:
                console.print(f"[green]✓[/] Tmux lifecycle hooks removed ({len(removed)} hooks)")
            else:
                console.print("[dim]Tmux lifecycle hooks not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove tmux hooks: {e}")

    # Uninstall all agent components (try both)
    _uninstall_claude_code()
    _uninstall_opencode()
    _uninstall_codex()
    _uninstall_gemini()
    _uninstall_antigravity()
    _uninstall_pi()

    # Remove config directory
    config_dir = Config.get_config_dir()
    if config_dir.exists():
        if yes or click.confirm("Remove ~/.repowire/ (config, logs, attachments)?", default=False):
            shutil.rmtree(config_dir)
            console.print("[green]✓[/] Removed ~/.repowire/")

    # Offer to uninstall the package itself
    pkg_mgr = _detect_package_manager()
    if pkg_mgr:
        if yes or click.confirm(f"Uninstall repowire package via {pkg_mgr}?", default=False):
            subprocess.run(_PKG_COMMANDS[pkg_mgr]["uninstall"])
            console.print(f"[green]✓[/] Package removed via {pkg_mgr}")

    console.print("")
    console.print("[green]Uninstall complete![/]")


def _uninstall_claude_code() -> None:
    """Uninstall Claude Code components."""
    import subprocess

    from repowire.installers.claude_code import uninstall_channel, uninstall_hooks

    # Remove hooks
    try:
        if uninstall_hooks():
            console.print("[green]✓[/] Claude Code hooks removed")
        else:
            console.print("[dim]Claude Code hooks not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove hooks: {e}")

    # Remove channel transport
    try:
        if uninstall_channel():
            console.print("[green]✓[/] Channel transport removed")
    except Exception:
        pass

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


def _uninstall_codex() -> None:
    """Uninstall Codex components."""
    from repowire.installers.codex import uninstall_hooks, uninstall_mcp

    try:
        if uninstall_hooks():
            console.print("[green]✓[/] Codex hooks removed")
        else:
            console.print("[dim]Codex hooks not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove Codex hooks: {e}")

    try:
        if uninstall_mcp():
            console.print("[green]✓[/] Codex MCP config removed")
    except Exception:
        pass


def _uninstall_gemini() -> None:
    """Uninstall Gemini components."""
    from repowire.installers.gemini import uninstall_hooks, uninstall_mcp

    try:
        if uninstall_hooks():
            console.print("[green]✓[/] Gemini hooks removed")
        else:
            console.print("[dim]Gemini hooks not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove Gemini hooks: {e}")

    try:
        if uninstall_mcp():
            console.print("[green]✓[/] Gemini MCP config removed")
    except Exception:
        pass


def _uninstall_antigravity() -> None:
    """Uninstall Antigravity CLI plugin."""
    from repowire.installers.antigravity import uninstall_hooks

    try:
        if uninstall_hooks():
            console.print("[green]✓[/] Antigravity plugin removed")
        else:
            console.print("[dim]Antigravity plugin not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove Antigravity plugin: {e}")


def _uninstall_pi() -> None:
    """Uninstall Pi components."""
    from repowire.installers.pi import uninstall_extension

    try:
        if uninstall_extension():
            console.print("[green]✓[/] Pi extension removed")
        else:
            console.print("[dim]Pi extension not installed[/]")
    except Exception as e:
        console.print(f"[yellow]![/] Failed to remove Pi extension: {e}")


@main.command()
@click.option("--post-upgrade", is_flag=True, hidden=True)
def update(post_upgrade: bool) -> None:
    """Update repowire to the latest version, reinstall hooks, restart daemon."""
    import os
    import shutil
    import subprocess

    from repowire.config.models import load_config

    if not post_upgrade:
        config = load_config()
        old_version = __version__
        pkg_mgr = _detect_package_manager()
        if not pkg_mgr:
            console.print("[red]Cannot detect how repowire was installed.[/]")
            console.print("Update manually: uv tool upgrade repowire / pip install -U repowire")
            return

        # Upgrade package first, then exec the upgraded CLI for hook/service work.
        package_spec = _repowire_package_spec(config)
        console.print(f"[cyan]Upgrading {package_spec} via {pkg_mgr}...[/]")
        result = subprocess.run(
            _upgrade_command(pkg_mgr, config),
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            console.print(f"[red]Upgrade failed:[/] {result.stderr[:200]}")
            return
        console.print(f"[green]✓[/] Upgraded (was {old_version})")

        repowire_bin = shutil.which("repowire")
        if not repowire_bin:
            console.print(
                "[yellow]![/] repowire is no longer on PATH; run repowire setup manually."
            )
            return
        console.print("[dim]Continuing with upgraded repowire...[/]")
        os.execv(repowire_bin, [repowire_bin, "update", "--post-upgrade"])

    # Reinstall hooks/plugins non-interactively, preserving current channel mode.
    config = load_config()
    _install_detected_backends(
        config,
        interactive=False,
        experimental_channels=False,
        preserve_channel_mode=True,
    )
    config.save()
    console.print("[green]✓[/] SQLite state enabled")
    console.print("  State migrations run automatically when the daemon restarts.")

    # Restart daemon service if running
    from repowire.service.installer import get_service_status, restart_service

    status = get_service_status()
    if status.get("running"):
        success, msg = restart_service()
        if success:
            console.print("[green]✓[/] Daemon service restarted")
            if _daemon_health_ok(config.daemon.host, config.daemon.port):
                console.print("[green]✓[/] Daemon health confirmed")
            else:
                console.print("[yellow]![/] Daemon restart requested, but health was not confirmed")
        else:
            console.print(f"[yellow]![/] Service restart: {msg}")

    console.print("[green]Update complete![/]")


@main.command()
def status() -> None:
    """Show repowire installation and daemon status."""
    import shutil

    import httpx

    from repowire.config.models import load_config
    from repowire.doctor import Status, check_update_availability
    from repowire.installers.claude_code import check_hooks_installed
    from repowire.service.installer import get_platform, get_service_status

    config = load_config()
    console.print("[cyan]Mode:[/] unified WebSocket")
    console.print(f"[cyan]Platform:[/] {get_platform()}")
    console.print("")

    update_result = check_update_availability(config)
    if update_result.status is Status.WARN:
        console.print(f"[yellow]![/] {update_result.name}: {update_result.detail}")
    elif update_result.status is Status.OK:
        console.print(f"[green]✓[/] {update_result.name}: {update_result.detail}")
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

    if shutil.which("codex") or (Path.home() / ".codex").exists():
        from repowire.installers.codex import check_hooks_installed as check_codex_hooks

        if check_codex_hooks():
            console.print("  [green]✓[/] codex (hooks installed)")
        else:
            console.print("  [yellow]✗[/] codex (hooks not installed)")
    else:
        console.print("  [dim]✗[/] codex (not detected)")

    if shutil.which("agy") or (Path.home() / ".gemini" / "antigravity-cli").exists():
        from repowire.installers.antigravity import (
            check_hooks_installed as check_agy_hooks,
        )

        if check_agy_hooks():
            console.print(
                "  [green]✓[/] antigravity (plugin installed; hook firing pending upstream)"
            )
        else:
            console.print("  [yellow]✗[/] antigravity (plugin not installed)")
    else:
        console.print("  [dim]✗[/] antigravity (not detected)")

    if shutil.which("pi") or (Path.home() / ".pi").exists():
        from repowire.installers.pi import check_extension_installed as check_pi_ext

        if check_pi_ext():
            console.print("  [green]✓[/] pi (extension installed)")
        else:
            console.print("  [yellow]✗[/] pi (extension not installed)")
    else:
        console.print("  [dim]✗[/] pi (not detected)")
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


# =============================================================================
# doctor command - diagnostic checks
# =============================================================================


_STATUS_GLYPHS = {
    "ok": ("[green]✓[/]", "green"),
    "warn": ("[yellow]⚠[/]", "yellow"),
    "fail": ("[red]✗[/]", "red"),
    "skip": ("[dim]·[/]", "dim"),
}


def _render_result(result: object, indent: int = 0) -> None:
    """Print a CheckResult and its children."""
    from repowire.doctor import CheckResult, Status  # local import to keep top tidy

    assert isinstance(result, CheckResult)
    glyph, _ = _STATUS_GLYPHS[result.status.value]
    pad = "  " * indent
    line = f"{pad}{glyph} {result.name}"
    if result.detail:
        # Dim the detail so the status+name pop
        line += f"  [dim]{result.detail}[/]"
    console.print(line)
    for child in result.children:
        _render_result(child, indent=indent + 1)
    # Mark unused import shim (Status referenced only via .value above)
    _ = Status


@main.command()
@click.option("--pane", "pane_id", default=None, help="tmux pane id to adopt (e.g. %42)")
@click.option("--backend", default=None, help="Agent running in the pane (required to link)")
@click.option("--name", default=None, help="Optional display name for the linked peer")
@click.option("--circle", default=None, help="Circle to register the peer into")
def link(pane_id: str | None, backend: str | None, name: str | None, circle: str | None) -> None:
    """Adopt an unregistered local tmux pane into the mesh.

    With no --pane: list candidate panes (those running an agent the daemon
    never registered) plus the command to adopt each.

    With --pane and --backend: register the pane AND establish its inbound
    ws-hook. The link succeeds only if a live connection is observed; otherwise
    it is rolled back (no half-registered ghost) and a retry hint is shown.
    """
    import sys

    import httpx

    daemon_url = _get_daemon_url()

    if not pane_id:
        try:
            with httpx.Client(timeout=3.0) as client:
                resp = client.get(f"{daemon_url}/panes/orphans")
                resp.raise_for_status()
                panes = resp.json().get("panes", [])
        except httpx.HTTPError as e:
            console.print(f"[red]✗[/] Could not reach daemon at {daemon_url}: {e}")
            sys.exit(1)
        if not panes:
            console.print("[green]✓[/] No unlinked panes found.")
            return
        console.print("[cyan]Unlinked tmux panes:[/]")
        for p in panes:
            hint = p["detected_backend"]
            tag = f"[green]{hint}[/]" if p["confidence"] == "hint" else "[dim]unknown[/]"
            console.print(
                f"  {p['pane_id']}  {tag}  [dim]{p['command']}  {p['cwd']}[/]"
            )
            console.print(
                f"    [dim]repowire link --pane {p['pane_id']} --backend "
                f"{hint if p['confidence'] == 'hint' else '<backend>'}[/]"
            )
        return

    if not backend:
        console.print(
            "[red]✗[/] --backend is required to link a pane "
            "(run `repowire link` with no args to see detected hints)."
        )
        sys.exit(2)

    payload: dict = {"backend": backend}
    if name:
        payload["name"] = name
    if circle:
        payload["circle"] = circle
    try:
        with httpx.Client(timeout=15.0) as client:
            resp = client.post(f"{daemon_url}/panes/{pane_id}/link", json=payload)
        if resp.status_code == 409:
            detail = resp.json().get("detail", {})
            console.print(f"[yellow]![/] {detail.get('hint', 'already linked')}")
            sys.exit(1)
        if resp.status_code == 422:
            console.print(f"[red]✗[/] {resp.json().get('detail', 'invalid request')}")
            sys.exit(2)
        resp.raise_for_status()
        body = resp.json()
    except httpx.HTTPError as e:
        console.print(f"[red]✗[/] Link request failed: {e}")
        sys.exit(1)

    if body.get("linked"):
        console.print(
            f"[green]✓[/] Linked {body['pane_id']} → "
            f"{body['display_name']} (transport connected)"
        )
        return
    console.print(f"[red]✗[/] Link not established: {body.get('reason')}")
    if body.get("repair_hint"):
        console.print(f"  [dim]{body['repair_hint']}[/]")
    sys.exit(1)


@main.command()
def doctor() -> None:
    """Run diagnostic checks and report repowire health."""
    from repowire.config.models import load_config
    from repowire.doctor import Status, exit_code, run_all

    config = load_config()
    daemon_url = _get_daemon_url()

    console.print("[cyan]repowire doctor[/]")
    console.print("")

    results = run_all(config, daemon_url)
    for r in results:
        _render_result(r)

    console.print("")

    code = exit_code(results)
    counts = {s: 0 for s in Status}

    def _walk(r: object) -> None:
        from repowire.doctor import CheckResult

        assert isinstance(r, CheckResult)
        counts[r.status] += 1
        for c in r.children:
            _walk(c)

    for r in results:
        _walk(r)

    summary = (
        f"[green]{counts[Status.OK]} ok[/]  "
        f"[yellow]{counts[Status.WARN]} warn[/]  "
        f"[red]{counts[Status.FAIL]} fail[/]  "
        f"[dim]{counts[Status.SKIP]} skip[/]"
    )
    console.print(summary)

    if code != 0:
        console.print("[red]doctor: failing checks present[/]")
    import sys

    sys.exit(code)


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


def _print_backend_install_messages(messages) -> None:
    for message in messages:
        style = {"success": "green", "warning": "yellow", "error": "red"}.get(
            message.level,
            "dim",
        )
        marker = {"success": "✓", "warning": "!", "error": "✗"}.get(message.level, " ")
        console.print(f"[{style}]{marker}[/] {message.text}")


def _install_detected_backends(
    config,
    *,
    interactive: bool,
    experimental_channels: bool,
    preserve_channel_mode: bool = False,
) -> list[str]:
    from repowire.agent_backends import AGENT_BACKENDS, BackendInstallOptions
    from repowire.agent_types import AgentType

    agents_setup: list[str] = []
    for backend_type, backend in AGENT_BACKENDS.items():
        if not backend.supports_local_spawn or not backend.detect_installed():
            continue
        use_channels = False
        if backend_type is AgentType.CLAUDE_CODE:
            if preserve_channel_mode:
                from repowire.installers.claude_code import check_channel_installed

                use_channels = check_channel_installed()
            else:
                use_channels = experimental_channels
                if not use_channels and interactive:
                    use_channels = click.confirm(
                        "Use experimental channel transport? (claude.ai login + bun)",
                        default=False,
                    )
        _print_backend_install_messages(
            backend.install(BackendInstallOptions(use_channels=use_channels))
        )
        agents_setup.append(backend_type.value)
        if backend.default_command is not None:
            config.daemon.spawn.commands.setdefault(backend_type, backend.default_command)
    return agents_setup


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


# =============================================================================
# codex command group - manages OpenAI Codex hooks
# =============================================================================


@main.group(hidden=True)
def codex() -> None:
    """Manage OpenAI Codex hooks."""
    pass


@codex.command(name="install")
def codex_install() -> None:
    """Install Repowire hooks into Codex."""
    from repowire.installers.codex import install_hooks, install_mcp

    try:
        install_hooks()
        install_mcp()
        console.print("[green]Codex hooks and MCP server installed successfully![/]")
    except Exception as e:
        console.print(f"[red]Failed to install Codex components: {e}[/]")


@codex.command(name="uninstall")
def codex_uninstall() -> None:
    """Remove Repowire hooks from Codex."""
    from repowire.installers.codex import uninstall_hooks, uninstall_mcp

    try:
        uninstall_hooks()
        uninstall_mcp()
        console.print("[green]Codex hooks and MCP server uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall Codex components: {e}[/]")


@codex.command(name="status")
def codex_status() -> None:
    """Check if Codex hooks are installed."""
    from repowire.installers.codex import check_hooks_installed, check_mcp_installed

    hooks_ok = check_hooks_installed()
    mcp_ok = check_mcp_installed()

    if hooks_ok and mcp_ok:
        console.print("[green]Codex hooks and MCP server are installed.[/]")
    elif hooks_ok:
        console.print("[yellow]Codex hooks are installed, but MCP server is missing.[/]")
    elif mcp_ok:
        console.print("[yellow]Codex MCP server is installed, but hooks are missing.[/]")
    else:
        console.print("[yellow]Codex components are not installed.[/]")
        console.print("Run 'repowire codex install' to set up.")


@main.group(hidden=True)
def gemini() -> None:
    """Manage Google Gemini CLI hooks."""
    pass


@gemini.command(name="install")
def gemini_install() -> None:
    """Install Repowire hooks into Gemini CLI."""
    from repowire.installers.gemini import install_hooks, install_mcp

    try:
        install_hooks()
        install_mcp()
        console.print("[green]Gemini hooks and MCP server installed![/]")
    except Exception as e:
        console.print(f"[red]Failed to install Gemini components: {e}[/]")


@gemini.command(name="uninstall")
def gemini_uninstall() -> None:
    """Remove Repowire hooks from Gemini CLI."""
    from repowire.installers.gemini import uninstall_hooks, uninstall_mcp

    try:
        uninstall_hooks()
        uninstall_mcp()
        console.print("[green]Gemini hooks and MCP server uninstalled.[/]")
    except Exception as e:
        console.print(f"[red]Failed to uninstall Gemini components: {e}[/]")


@gemini.command(name="status")
def gemini_status() -> None:
    """Check if Gemini CLI hooks are installed."""
    from repowire.installers.gemini import check_hooks_installed, check_mcp_installed

    hooks_ok = check_hooks_installed()
    mcp_ok = check_mcp_installed()

    if hooks_ok and mcp_ok:
        console.print("[green]Gemini hooks and MCP server are installed.[/]")
    elif hooks_ok:
        console.print("[yellow]Gemini hooks installed, but MCP server missing.[/]")
    elif mcp_ok:
        console.print("[yellow]Gemini MCP server installed, but hooks missing.[/]")
    else:
        console.print("[yellow]Gemini components not installed.[/]")
        console.print("Run 'repowire gemini install' to set up.")


# =============================================================================
# orchestrator command group - scaffold + launch persistent orchestrator peer
# =============================================================================


@main.group()
def orchestrator() -> None:
    """Scaffold + launch the orchestrator workspace + peer."""
    pass


# Runtime preference order for orchestrator. Pi is preferred when installed
# (it's the orchestrator-shaped runtime). Falls back through the others.
_ORCHESTRATOR_RUNTIME_PREFERENCE = (
    "pi",
    "claude-code",
    "codex",
    "gemini",
    "opencode",
)

# Spawn command per backend. Codex needs the bypass flag or warmup is eaten
# by approval prompts.
_ORCHESTRATOR_RUNTIME_COMMANDS = {
    "pi": "pi",
    "claude-code": "claude --dangerously-skip-permissions",
    "codex": "codex --dangerously-bypass-approvals-and-sandbox",
    "gemini": "gemini --yolo",
    "opencode": "opencode",
}


def _configured_spawn_command(backend: str, profile: str | None = None) -> str | None:
    """Return the configured spawn command for a backend/runtime profile."""
    from repowire.config.models import AgentType, apply_spawn_profile, load_config

    try:
        backend_type = AgentType(backend)
    except ValueError:
        return None
    spawn_cfg = load_config().daemon.spawn
    command = spawn_cfg.commands.get(backend_type)
    if command is None:
        return None
    if not profile:
        return command
    selected_profile = spawn_cfg.profiles.get(backend_type, {}).get(profile)
    if selected_profile is None:
        return None
    return apply_spawn_profile(command, selected_profile)


def _detect_runtime_for_orchestrator() -> str | None:
    """Pick a runtime in preference order. Returns name or None if none found.

    Pi only wins if its repowire extension is installed — otherwise the spawned
    pi session won't join the mesh and the orchestrator would be orphaned.
    """
    import shutil as _shutil

    from repowire.installers.pi import check_extension_installed as _pi_extension_installed

    if (_shutil.which("pi") or (Path.home() / ".pi").exists()) and _pi_extension_installed():
        return "pi"
    for name, exe in (
        ("claude-code", "claude"),
        ("codex", "codex"),
        ("gemini", "gemini"),
        ("opencode", "opencode"),
    ):
        if _shutil.which(exe):
            return name
    return None


def _load_orchestrator_yaml() -> dict:
    """Load orchestrator.yaml from the workspace if present. Empty dict on miss."""
    import yaml

    from repowire.orchestrator import workspace_path

    candidate = workspace_path() / "orchestrator.yaml"
    if not candidate.is_file():
        return {}
    try:
        with candidate.open() as f:
            data = yaml.safe_load(f) or {}
        return data if isinstance(data, dict) else {}
    except Exception as e:
        console.print(f"[yellow]Warning: could not parse orchestrator.yaml: {e}[/]")
        return {}


@orchestrator.command(name="init")
@click.option("--force", is_flag=True, help="Re-render workspace, backing up the current one")
def orchestrator_init(force: bool) -> None:
    """Scaffold ~/.repowire/orchestrator/ from the bundled template."""
    from repowire.orchestrator import init_workspace

    rendered, msg = init_workspace(force=force)
    if rendered:
        console.print(f"[green]✓[/] {msg}")
        console.print("[dim]Next:[/] [cyan]repowire orchestrator start[/]")
    else:
        console.print(f"[yellow]![/] {msg}")


@orchestrator.command(name="diff")
def orchestrator_diff() -> None:
    """Diff bundled template vs. workspace for repowire-owned files (read-only).

    Surfaces what changed in the shipped template since you initialized.
    Apply changes manually with cp, or `repowire orchestrator init --force` to
    re-render fresh (backs up current workspace first).
    """
    import difflib

    from repowire.orchestrator import update_workspace, workspace_path
    from repowire.orchestrator.workspace import _template_root

    if not (workspace_path() / "AGENTS.md").exists():
        console.print("[red]Workspace not initialized.[/] Run [cyan]repowire orchestrator init[/].")
        return

    report = update_workspace()
    repowire_owned_changes = [(p, s) for p, s in report if s != "identical"]
    if not repowire_owned_changes:
        console.print("[green]✓[/] Workspace is up to date with bundled template.")
        return

    console.print(f"[cyan]Found {len(repowire_owned_changes)} differences:[/]")
    ws = workspace_path()
    with _template_root() as template_root:
        for rel, status in repowire_owned_changes:
            console.print(f"\n[bold]{rel}[/] — [yellow]{status}[/]")
            if status == "symlink-broken":
                console.print("  Run [cyan]repowire orchestrator init --force[/] to recreate.")
                continue
            local = ws / rel
            shipped = template_root / rel
            if status == "missing":
                console.print(f"  File absent locally. Shipped at {shipped}")
                continue
            # differs — show a short diff
            local_lines = local.read_text().splitlines(keepends=True)
            shipped_lines = shipped.read_text().splitlines(keepends=True)
            diff = list(
                difflib.unified_diff(
                    local_lines,
                    shipped_lines,
                    fromfile=f"local:{rel}",
                    tofile=f"shipped:{rel}",
                    n=2,
                )
            )
            for line in diff[:30]:
                console.print(line.rstrip(), highlight=False)
            if len(diff) > 30:
                console.print(f"  [dim]... {len(diff) - 30} more lines; diff truncated[/]")
            console.print(f"  [dim]To accept upstream: cp the file from {shipped} → {local}[/]")


@orchestrator.command(name="start")
@click.option(
    "--runtime",
    type=click.Choice(list(_ORCHESTRATOR_RUNTIME_PREFERENCE)),
    default=None,
    help="Override the auto-detected runtime",
)
@click.option("--profile", help="Named spawn profile to apply to the runtime command")
@click.option(
    "--service",
    is_flag=True,
    help="(v2) Install as launchd/systemd service. Currently a placeholder.",
)
def orchestrator_start(runtime: str | None, profile: str | None, service: bool) -> None:
    """Spawn the orchestrator peer in its workspace."""
    import httpx

    from repowire.config.models import AgentType
    from repowire.orchestrator import (
        init_workspace,
        validate_workspace,
        workspace_path,
    )

    if service:
        console.print(
            "[yellow]--service mode is planned for v2; falling back to manual tmux launch.[/]"
        )

    ws = workspace_path()

    # Preflight 1: workspace
    if not (ws / "AGENTS.md").exists():
        console.print(
            f"[yellow]Workspace not initialized at {ws}.[/] Running [cyan]orchestrator init[/]..."
        )
        rendered, msg = init_workspace()
        if rendered:
            console.print(f"[green]✓[/] {msg}")
        else:
            console.print(f"[red]✗[/] {msg}")
            return

    try:
        from repowire.orchestrator import load_active_soul, sync_active_soul_shim

        sync_active_soul_shim(load_active_soul())
    except Exception as e:
        console.print(f"[yellow]Warning: could not sync SOUL.md shim: {e}[/]")

    ok, errors = validate_workspace()
    if not ok:
        console.print("[red]Workspace validation failed:[/]")
        for e in errors:
            console.print(f"  [red]✗[/] {e}")
        console.print("Try [cyan]repowire orchestrator init --force[/] to recreate.")
        return

    # Preflight 2: daemon reachable
    daemon_url = _get_daemon_url()
    try:
        with httpx.Client(timeout=2.0) as client:
            resp = client.get(f"{daemon_url}/health")
            resp.raise_for_status()
    except httpx.ConnectError:
        console.print(
            f"[red]✗[/] Daemon not reachable at {daemon_url}. "
            "Start it with [cyan]repowire serve[/] (or "
            "[cyan]repowire service install[/] for auto-start)."
        )
        return
    except Exception as e:
        console.print(f"[red]✗[/] Daemon health check failed: {e}")
        return

    # Preflight 3: runtime selection
    yaml_config = _load_orchestrator_yaml()
    selected_runtime = runtime or yaml_config.get("runtime") or _detect_runtime_for_orchestrator()
    if not selected_runtime:
        console.print(
            "[red]✗[/] No supported runtime found on PATH. "
            "Install one of: pi, claude, codex, gemini, opencode."
        )
        return
    if selected_runtime not in _ORCHESTRATOR_RUNTIME_COMMANDS:
        console.print(f"[red]✗[/] Unknown runtime: {selected_runtime}")
        return

    circle = yaml_config.get("circle") or "default"
    backend = AgentType(selected_runtime)
    yaml_command = yaml_config.get("command")
    selected_profile = profile or yaml_config.get("profile")
    command = yaml_command or _configured_spawn_command(
        selected_runtime,
        selected_profile,
    )
    if not command:
        suggested = _ORCHESTRATOR_RUNTIME_COMMANDS[selected_runtime]
        if selected_profile:
            console.print(
                f"[red]✗[/] Orchestrator spawn profile is not configured for {selected_runtime!r}."
            )
            console.print(
                f"Add [cyan]daemon.spawn.profiles.{selected_runtime}.{selected_profile}[/] "
                "to ~/.repowire/config.yaml or omit --profile."
            )
        else:
            console.print(
                "[red]✗[/] Orchestrator runtime is not configured in daemon.spawn.commands."
            )
            console.print(
                f"Add [cyan]daemon.spawn.commands.{selected_runtime}: "
                f"{suggested!r}[/] to ~/.repowire/config.yaml."
            )
        return

    profile_label = "n/a (command override)" if yaml_command else selected_profile or "default"
    console.print(
        f"[cyan]Spawning orchestrator[/] "
        f"(runtime={selected_runtime}, circle={circle}, "
        f"profile={profile_label}, role=orchestrator)..."
    )
    try:
        with httpx.Client(timeout=30.0) as client:
            body: dict[str, object] = {
                "path": str(ws),
                "circle": circle,
                "role": "orchestrator",
            }
            if yaml_command:
                body["command"] = command
            else:
                body["backend"] = backend.value
                if selected_profile:
                    body["profile"] = selected_profile
            resp = client.post(
                f"{daemon_url}/spawn",
                json=body,
                headers=_auth_headers(),
            )
            resp.raise_for_status()
            result = resp.json()
    except Exception as e:
        console.print(f"[red]✗[/] Spawn failed: {e}")
        return

    console.print(f"[green]✓[/] Orchestrator spawned at [cyan]{result['tmux_session']}[/]")
    console.print(f"  Attach: [cyan]tmux attach -t {circle}[/]")
    console.print(f"  Dashboard: {daemon_url}/dashboard")
    try:
        from repowire.orchestrator import load_active_soul

        soul = load_active_soul()
        if soul is not None:
            console.print(
                f"  Persona: [cyan]{soul.name}[/] ([dim]{soul.source}[/], sha256:{soul.short_hash})"
            )
    except Exception:
        pass


# -- orchestrator persona subgroup -------------------------------------------


@orchestrator.group(name="persona")
def orchestrator_persona() -> None:
    """Manage orchestrator persona SOUL.md files (experimental)."""
    pass


@orchestrator_persona.command(name="list")
def orchestrator_persona_list() -> None:
    """List discoverable personas in workspace + global persona dirs."""
    from repowire.orchestrator import list_personas

    rows = list_personas()
    if not rows:
        console.print("[dim]No personas found.[/]")
        console.print(
            "Drop a SOUL.md at [cyan]~/.repowire/personas/<name>/SOUL.md[/] to get started."
        )
        return
    for row in rows:
        marker = "[green]*[/]" if row.active else " "
        console.print(
            f"{marker} [cyan]{row.name}[/] "
            f"[dim]({row.source}, sha256:{row.short_hash})[/]\n"
            f"    {row.path}"
        )


@orchestrator_persona.command(name="show")
@click.argument("name", required=False)
def orchestrator_persona_show(name: str | None) -> None:
    """Show resolved SOUL.md for NAME (or the active persona)."""
    from repowire.orchestrator import get_active_persona, load_soul

    target = name or get_active_persona()
    if not target:
        console.print("[yellow]No persona named and no active persona set.[/]")
        return
    soul = load_soul(target)
    if soul is None:
        console.print(f"[red]✗[/] No SOUL.md found for persona {target!r}")
        return
    console.print(f"[cyan]{soul.name}[/] [dim]({soul.source}, sha256:{soul.short_hash})[/]")
    console.print(f"[dim]{soul.path}[/]")
    console.print()
    console.print(soul.content.strip())


@orchestrator_persona.command(name="use")
@click.argument("name")
def orchestrator_persona_use(name: str) -> None:
    """Mark NAME as the active workspace persona."""
    from repowire.orchestrator import set_active_persona

    try:
        soul = set_active_persona(name)
    except FileNotFoundError as e:
        console.print(f"[red]✗[/] {e}")
        return
    except ValueError as e:
        console.print(f"[red]✗[/] {e}")
        return
    console.print(
        f"[green]✓[/] Active persona set to [cyan]{soul.name}[/] "
        f"[dim](source={soul.source}, sha256:{soul.short_hash})[/]"
    )


@orchestrator_persona.command(name="clear")
def orchestrator_persona_clear() -> None:
    """Remove the active persona marker."""
    from repowire.orchestrator import clear_active_persona

    if clear_active_persona():
        console.print("[green]✓[/] Active persona cleared.")
    else:
        console.print("[dim]No active persona was set.[/]")


@orchestrator_persona.command(name="path")
@click.argument("name", required=False)
def orchestrator_persona_path(name: str | None) -> None:
    """Print resolved SOUL.md path for NAME (or active persona)."""
    from repowire.orchestrator import find_soul_path, get_active_persona

    target = name or get_active_persona()
    if not target:
        console.print("[yellow]No persona named and no active persona set.[/]")
        return
    path, source = find_soul_path(target)
    if path is None:
        console.print(f"[red]✗[/] No SOUL.md found for persona {target!r}")
        return
    console.print(f"{path} [dim]({source})[/]")


_MEMORY_SCOPE_CHOICES = [
    "global",
    "user",
    "project",
    "projects",
    "persona",
    "personas",
    "orchestrator",
]


def _memory_scope_options(fn: Any) -> Any:
    fn = click.option(
        "--persona",
        help="Persona name for --scope persona (defaults to active persona)",
    )(fn)
    fn = click.option(
        "--project",
        help="Project name for project scope",
    )(fn)
    fn = click.option(
        "--scope",
        default=None,
        type=click.Choice(_MEMORY_SCOPE_CHOICES),
        help="Memory scope. Defaults to orchestrator in that workspace, otherwise user.",
    )(fn)
    return fn


def _resolve_memory_scope(
    scope: str | None,
    project: str | None,
    persona: str | None,
) -> Any:
    from repowire.memory import resolve_scope_path

    return resolve_scope_path(scope, project=project, persona=persona)


@main.group()
def memory() -> None:
    """Inspect and write explicit filesystem-backed mesh memory."""
    pass


@memory.command(name="path")
@_memory_scope_options
def memory_path(scope: str | None, project: str | None, persona: str | None) -> None:
    """Print the resolved memory directory for a scope."""
    try:
        label, path = _resolve_memory_scope(scope, project, persona)
    except ValueError as e:
        raise click.ClickException(str(e)) from e
    console.print(f"{path} [dim]({label})[/]")


@memory.command(name="list")
@_memory_scope_options
def memory_list(scope: str | None, project: str | None, persona: str | None) -> None:
    """List memory entries in a scope."""
    from repowire.memory import list_memories, resolve_scope_path

    try:
        label, path = resolve_scope_path(scope, project=project, persona=persona)
        rows = list_memories(scope, project=project, persona=persona)
    except ValueError as e:
        raise click.ClickException(str(e)) from e
    if not rows:
        console.print(f"[dim]No memories found in {label} ({path}).[/]")
        return
    table = Table(show_header=True, header_style="bold cyan")
    table.add_column("Slug")
    table.add_column("Type")
    table.add_column("Description")
    table.add_column("Updated")
    for row in rows:
        table.add_row(row.slug, row.type, row.description, row.updated_at)
    console.print(table)


@memory.command(name="show")
@click.argument("slug")
@_memory_scope_options
def memory_show(
    slug: str,
    scope: str | None,
    project: str | None,
    persona: str | None,
) -> None:
    """Show a memory Markdown file by slug."""
    from repowire.memory import read_memory

    try:
        content, path = read_memory(slug, scope, project=project, persona=persona)
    except (FileNotFoundError, ValueError) as e:
        raise click.ClickException(str(e)) from e
    console.print(f"[dim]{path}[/]")
    console.print()
    console.print(content.rstrip())


@memory.command(name="search")
@click.argument("query")
@click.option("--all", "all_scopes_flag", is_flag=True, help="Search every scope")
@_memory_scope_options
def memory_search(
    query: str,
    all_scopes_flag: bool,
    scope: str | None,
    project: str | None,
    persona: str | None,
) -> None:
    """Search memory files by text."""
    from repowire.memory import search_memories

    try:
        rows = search_memories(
            query,
            scope,
            project=project,
            persona=persona,
            all_scopes=all_scopes_flag,
        )
    except ValueError as e:
        raise click.ClickException(str(e)) from e
    if not rows:
        console.print("[dim]No memory matches found.[/]")
        return
    for row in rows:
        console.print(f"[cyan]{row.scope}/{row.slug}:{row.line}[/] {row.snippet}")


@memory.command(name="write")
@click.argument("slug")
@click.option("--body", required=True, help="Markdown body to write")
@click.option("--type", "memory_type", default="reference", show_default=True)
@click.option("--description", default="", help="One-line memory summary")
@click.option("--append", "append", is_flag=True, help="Append to an existing memory")
@click.option("--force", is_flag=True, help="Overwrite an existing memory")
@_memory_scope_options
def memory_write(
    slug: str,
    body: str,
    memory_type: str,
    description: str,
    append: bool,
    force: bool,
    scope: str | None,
    project: str | None,
    persona: str | None,
) -> None:
    """Write one memory Markdown file explicitly."""
    from repowire.memory import write_memory

    if append and force:
        raise click.ClickException("--append and --force are mutually exclusive")
    try:
        path = write_memory(
            slug,
            body,
            scope,
            project=project,
            persona=persona,
            memory_type=memory_type,
            description=description,
            append=append,
            overwrite=force,
        )
    except FileExistsError as e:
        raise click.ClickException(f"memory already exists: {e.filename}") from e
    except ValueError as e:
        raise click.ClickException(str(e)) from e
    console.print(f"[green]Wrote[/] {path}")


@main.group()
def peer() -> None:
    """Manage peers in the mesh."""
    pass


def _get_daemon_url() -> str:
    """Get the daemon HTTP URL from config."""
    from repowire.config.models import load_config

    config = load_config()
    return f"http://{config.daemon.host}:{config.daemon.port}"


def _parse_schedule_when(raw: str) -> str:
    """Parse CLI schedule time: ISO-8601 or compact relative duration."""
    import re
    from datetime import datetime, timedelta, timezone

    text = raw.strip()
    rel = text
    if rel.lower().startswith("in "):
        rel = rel[3:].strip()
    match = re.fullmatch(r"(?i)(\d+)\s*([smhdw])", rel)
    if match:
        amount = int(match.group(1))
        unit = match.group(2).lower()
        seconds = {
            "s": amount,
            "m": amount * 60,
            "h": amount * 3600,
            "d": amount * 86400,
            "w": amount * 604800,
        }[unit]
        return (datetime.now(timezone.utc) + timedelta(seconds=seconds)).isoformat()
    return text


def _current_cli_peer_name(client: Any | None = None) -> str:
    """Best-effort current peer name for CLI self scheduling."""
    import os
    from urllib.parse import quote

    import httpx

    if os.environ.get("REPOWIRE_DISPLAY_NAME"):
        return os.environ["REPOWIRE_DISPLAY_NAME"]
    if os.environ.get("REPOWIRE_PEER_ID"):
        return os.environ["REPOWIRE_PEER_ID"]

    pane_id = os.environ.get("TMUX_PANE")
    if pane_id:
        owns_client = client is None
        http_client = client or httpx.Client(timeout=5.0)
        try:
            resp = http_client.get(
                f"{_get_daemon_url()}/peers/by-pane/{quote(pane_id, safe='')}",
            )
            if resp.status_code < 400:
                body = resp.json()
                name = body.get("display_name") or body.get("name") or body.get("peer_id")
                if name:
                    return name
        finally:
            if owns_client:
                http_client.close()

    from repowire.hooks.utils import get_display_name

    return get_display_name()


_AGENT_SCAFFOLD_FILES = ("AGENTS.md", "CLAUDE.md", "README.md")
_AGENT_RUNTIME_LINKS = ("CLAUDE.md",)


def _validate_agent_name(name: str) -> str:
    import re

    cleaned = name.strip()
    if not re.fullmatch(r"[a-zA-Z0-9._-]+", cleaned):
        raise click.ClickException("Agent name must match ^[a-zA-Z0-9._-]+$")
    return cleaned


def _repo_or_cwd() -> Path:
    import subprocess

    try:
        result = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            cwd=Path.cwd(),
            check=True,
            capture_output=True,
            text=True,
        )
        root = result.stdout.strip()
        if root:
            return Path(root).resolve()
    except Exception:
        pass
    return Path.cwd().resolve()


def _default_agent_path(name: str) -> Path:
    return _repo_or_cwd() / ".repowire" / "agents" / name


def _backup_existing_agent_dir(path: Path) -> Path:
    import shutil
    from datetime import datetime, timezone

    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup = path.parent / f"{path.name}.bak.{timestamp}"
    counter = 1
    while backup.exists():
        backup = path.parent / f"{path.name}.bak.{timestamp}.{counter}"
        counter += 1
    shutil.move(str(path), str(backup))
    return backup


def _agent_scaffold_agents_md(name: str) -> str:
    return f"""# {name}

You are the `{name}` Repowire worker.

## Purpose

Describe the standing job this worker owns.

## Standing Context

- Keep durable job state honest.
- Prefer explicit progress updates over silent work.
- Store secrets outside this folder and outside Repowire job records.

## Durable Job Protocol

When Repowire dispatches a job here, the prompt includes `job_id` and
`attempt_id`. Use those exact values in lifecycle updates. Acknowledging an ask
only confirms receipt; completion requires a terminal `repowire jobs update`
for the current attempt.
"""


def _agent_scaffold_readme(name: str, path: Path, backend: str | None) -> str:
    backend_arg = f" --backend {backend}" if backend else " --backend <runtime>"
    return f"""# {name}

This is a Repowire agent folder. Repowire does not register it globally; jobs
target this folder directly with `--path`.

```bash
repowire jobs create "{name}" --path {path}{backend_arg} --cron "@daily" --prompt "Run the worker."
```

`AGENTS.md` is the source of truth. `CLAUDE.md` points back to it for Claude
Code; other supported runtimes load `AGENTS.md` directly.
"""


def _path_is_git_ignored(path: Path) -> bool:
    import subprocess

    try:
        result = subprocess.run(
            ["git", "check-ignore", "-q", str(path)],
            cwd=Path.cwd(),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        return result.returncode == 0
    except Exception:
        return False


def _path_under_spawn_allowed_paths(path: Path) -> bool:
    from repowire.config.models import load_config

    try:
        cfg = load_config()
        roots = cfg.daemon.spawn.allowed_paths
    except Exception:
        return False
    if not roots:
        return False
    resolved = path.expanduser().resolve()
    for root_raw in roots:
        root = Path(root_raw).expanduser().resolve()
        if resolved == root or root in resolved.parents:
            return True
    return False


@main.group()
def agents() -> None:
    """Create local worker folders for durable jobs."""
    pass


@agents.command(name="create")
@click.argument("name")
@click.option("--path", "path_raw", help="Scaffold path (default: .repowire/agents/NAME)")
@click.option(
    "--backend",
    type=click.Choice(["claude-code", "codex", "gemini", "antigravity", "opencode", "pi"]),
    help="Backend to include in the suggested jobs command",
)
@click.option("--force", is_flag=True, help="Back up and recreate an existing scaffold")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON instead of text")
def agents_create_cmd(
    name: str,
    path_raw: str | None,
    backend: str | None,
    force: bool,
    as_json: bool,
) -> None:
    """Create a repo-local Repowire agent folder scaffold."""
    clean_name = _validate_agent_name(name)
    target = (
        Path(path_raw).expanduser() if path_raw else _default_agent_path(clean_name)
    ).resolve()

    backup: Path | None = None
    created = True
    symlink_fallback = False

    if target.exists():
        if target.is_file() and not force:
            raise click.ClickException(f"Scaffold path exists and is not a directory: {target}")
        if (target / "AGENTS.md").exists() and not force:
            created = False
        elif not force:
            collisions = [
                filename for filename in _AGENT_SCAFFOLD_FILES if (target / filename).exists()
            ]
            if collisions:
                joined = ", ".join(collisions)
                raise click.ClickException(
                    f"Scaffold files already exist at {target}: {joined}. "
                    "Pass --force to back up and recreate."
                )
            target.mkdir(parents=True, exist_ok=True)
        else:
            backup = _backup_existing_agent_dir(target)

    if created:
        target.mkdir(parents=True, exist_ok=True)
        agents_md = target / "AGENTS.md"
        agents_md.write_text(_agent_scaffold_agents_md(clean_name), encoding="utf-8")
        for link_name in _AGENT_RUNTIME_LINKS:
            link = target / link_name
            if link.exists() or link.is_symlink():
                link.unlink()
            try:
                link.symlink_to("AGENTS.md")
            except OSError:
                symlink_fallback = True
                link.write_text(agents_md.read_text(encoding="utf-8"), encoding="utf-8")
        (target / "README.md").write_text(
            _agent_scaffold_readme(clean_name, target, backend),
            encoding="utf-8",
        )

    git_ignored = _path_is_git_ignored(target)
    spawn_allowed = _path_under_spawn_allowed_paths(target)
    suggested = (
        f"repowire jobs create \"{clean_name}\" --path {target}"
        f"{f' --backend {backend}' if backend else ' --backend <runtime>'}"
        ' --cron "@daily" --prompt "Run the worker."'
    )
    payload = {
        "name": clean_name,
        "path": str(target),
        "created": created,
        "backup": str(backup) if backup else None,
        "symlink_fallback": symlink_fallback,
        "git_ignored": git_ignored,
        "spawn_allowed": spawn_allowed,
        "suggested_job_command": suggested,
    }
    if as_json:
        console.print_json(data=payload)
        return

    verb = "Created" if created else "Agent folder already exists"
    console.print(f"[green]{verb}[/] [cyan]{target}[/]")
    if backup:
        console.print(f"  backup: [cyan]{backup}[/]")
    if symlink_fallback:
        console.print("  [yellow]runtime files were copied because symlinks failed[/]")
    if git_ignored:
        console.print(
            "  [yellow]note:[/] this path is git-ignored; "
            "unignore it if you want to share it"
        )
    if not spawn_allowed:
        console.print(
            "  [yellow]note:[/] this path is not under daemon.spawn.allowed_paths; "
            "add an allowed path before daemon-spawned jobs can run it"
        )
    click.echo(f"  job: {suggested}")


@main.group()
def jobs() -> None:
    """Create and inspect durable tracked work jobs."""
    pass


@jobs.command(name="create")
@click.argument("title")
@click.option("--kind", default="general", show_default=True)
@click.option("--created-by", "created_by_peer_id", help="Creator/requester peer id")
@click.option("--owner", "owner_peer_id", help="Owning peer or operator id")
@click.option("--assigned-peer", "assigned_peer_id", help="Assigned executor peer id")
@click.option("--session", "repowire_session_id", help="Related Repowire session id")
@click.option("--correlation-id", help="Related ask/query correlation id")
@click.option("--circle", "-c", help="Visibility/routing circle")
@click.option("--source-kind", default="cli", show_default=True)
@click.option("--source-id", help="Source-local identifier")
@click.option("--scope", help="Small routing/display scope label")
@click.option("--prompt", help="Inline prompt to dispatch")
@click.option("--prompt-file", help="Read prompt from file")
@click.option("--path", help="Target project path for spawned peer")
@click.option("--backend", help="Backend to spawn when no assigned peer is provided")
@click.option("--profile", help="Spawn profile")
@click.option("--due-at", help="Schedule due time (ISO-8601)")
@click.option("--cron", help="Recurring cron expression or alias")
@click.option("--result-surface", help="Metadata-only result surface")
@click.option(
    "--process-scope",
    type=click.Choice(["per-fire", "per_fire", "persistent"]),
    help="Executor process lifetime for path/backend jobs",
)
@click.option(
    "--continuity",
    type=click.Choice(["resume", "fresh"]),
    help="Backend-native context continuity between fires",
)
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_create_cmd(
    title: str,
    kind: str,
    created_by_peer_id: str | None,
    owner_peer_id: str | None,
    assigned_peer_id: str | None,
    repowire_session_id: str | None,
    correlation_id: str | None,
    circle: str | None,
    source_kind: str,
    source_id: str | None,
    scope: str | None,
    prompt: str | None,
    prompt_file: str | None,
    path: str | None,
    backend: str | None,
    profile: str | None,
    due_at: str | None,
    cron: str | None,
    result_surface: str | None,
    process_scope: str | None,
    continuity: str | None,
    as_json: bool,
) -> None:
    """Create a durable job without dispatching it to an executor."""
    import httpx
    if due_at and cron:
        raise click.ClickException("Pass --due-at or --cron, not both.")

    body = {
        "title": title,
        "kind": kind,
        "source_kind": source_kind,
    }
    for key, value in {
        "created_by_peer_id": created_by_peer_id,
        "owner_peer_id": owner_peer_id,
        "assigned_peer_id": assigned_peer_id,
        "repowire_session_id": repowire_session_id,
        "correlation_id": correlation_id,
        "circle": circle,
        "source_id": source_id,
        "scope": scope,
        "prompt": prompt,
        "prompt_file": prompt_file,
        "path": path,
        "backend": backend,
        "profile": profile,
        "due_at": _parse_schedule_when(due_at) if due_at else None,
        "cron": cron,
        "result_surface": result_surface,
        "process_scope": process_scope,
        "continuity": continuity,
    }.items():
        if value:
            body[key] = value

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(f"{_get_daemon_url()}/jobs", json=body)
            resp.raise_for_status()
            payload = resp.json()
            if as_json:
                console.print_json(data=payload.get("calendar") or payload.get("status") or payload)
            elif payload.get("calendar"):
                _print_calendar_status(payload["calendar"], created=True)
            else:
                _print_job_status(payload["status"], created=True)
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to create job: {_http_error_detail(e)}") from e


@jobs.command(name="list")
@click.option("--state", help="Filter by lifecycle state")
@click.option("--owner", "owner_peer_id", help="Filter by owner peer id")
@click.option("--created-by", "created_by_peer_id", help="Filter by creator peer id")
@click.option("--session", "repowire_session_id", help="Filter by Repowire session id")
@click.option("--circle", "-c", help="Filter by circle")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON list instead of table")
def jobs_list_cmd(
    state: str | None,
    owner_peer_id: str | None,
    created_by_peer_id: str | None,
    repowire_session_id: str | None,
    circle: str | None,
    as_json: bool,
) -> None:
    """List durable jobs."""
    import httpx

    params = {
        key: value
        for key, value in {
            "state": state,
            "owner_peer_id": owner_peer_id,
            "created_by_peer_id": created_by_peer_id,
            "repowire_session_id": repowire_session_id,
            "circle": circle,
        }.items()
        if value
    }
    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/jobs", params=params or None)
            resp.raise_for_status()
            payload = resp.json()
            items = payload.get("work", [])
            recurring = payload.get("recurring", [])
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to list jobs: {_http_error_detail(e)}") from e

    if as_json:
        console.print_json(data={"work": items, "recurring": recurring})
        return

    if not items and not recurring:
        console.print("[yellow]No jobs.[/]")
        return
    if recurring:
        recurring_table = Table(title="Recurring Jobs")
        recurring_table.add_column("ID", style="cyan")
        recurring_table.add_column("State")
        recurring_table.add_column("Title")
        recurring_table.add_column("Cron")
        recurring_table.add_column("Next Due")
        recurring_table.add_column("Last Job")
        for item in recurring:
            recurring_table.add_row(
                item.get("calendar_id") or item.get("recurring_id", ""),
                item.get("state", ""),
                item.get("title", ""),
                item.get("cron", ""),
                item.get("next_due_at") or "-",
                item.get("last_occurrence_work_id") or "-",
            )
        console.print(recurring_table)
    if items:
        table = Table(title="Repowire Jobs")
        table.add_column("ID", style="cyan")
        table.add_column("State")
        table.add_column("Title")
        table.add_column("Kind")
        table.add_column("Owner")
        table.add_column("Due")
        table.add_column("Updated")
        for item in items:
            table.add_row(
                item.get("job_id") or item.get("work_id", ""),
                item.get("state", ""),
                item.get("title", ""),
                item.get("kind", ""),
                item.get("owner_peer_id") or "-",
                item.get("due_at") or "-",
                item.get("updated_at", ""),
            )
        console.print(table)


@jobs.command(name="show")
@click.argument("job_id")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_show_cmd(job_id: str, as_json: bool) -> None:
    """Show a job status record."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/jobs/{job_id}/status")
            if resp.status_code == 404:
                raise click.ClickException(f"No job: {job_id}")
            resp.raise_for_status()
            status = resp.json()["status"]
            if as_json:
                console.print_json(data=status)
            elif status.get("calendar_id"):
                _print_calendar_status(status)
            else:
                _print_job_status(status)
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to show job: {_http_error_detail(e)}") from e


@jobs.command(name="update")
@click.argument("job_id")
@click.option("--state", required=True, help="New lifecycle state")
@click.option("--reason", "state_reason", help="Machine-readable state reason")
@click.option("--phase", help="Display phase")
@click.option("--note", "progress_note", help="Append a progress note")
@click.option("--result-summary", help="Terminal result summary")
@click.option("--attempt-id", help="Current runner attempt id for runner-managed jobs")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_update_cmd(
    job_id: str,
    state: str,
    state_reason: str | None,
    phase: str | None,
    progress_note: str | None,
    result_summary: str | None,
    attempt_id: str | None,
    as_json: bool,
) -> None:
    """Update job lifecycle status and optionally append a progress note."""
    import httpx

    body = {"state": state}
    for key, value in {
        "state_reason": state_reason,
        "phase": phase,
        "progress_note": progress_note,
        "result_summary": result_summary,
        "attempt_id": attempt_id,
    }.items():
        if value:
            body[key] = value
    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.patch(f"{_get_daemon_url()}/jobs/{job_id}", json=body)
            if resp.status_code == 404:
                raise click.ClickException(f"No job: {job_id}")
            resp.raise_for_status()
            status = resp.json()["status"]
            if as_json:
                console.print_json(data=status)
            elif status.get("calendar_id"):
                _print_calendar_status(status)
            else:
                _print_job_status(status)
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to update job: {_http_error_detail(e)}") from e


def _post_job_action(job_id: str, action: str, *, as_json: bool) -> None:
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(f"{_get_daemon_url()}/jobs/{job_id}/{action}", json={})
            if resp.status_code == 404:
                raise click.ClickException(f"No job: {job_id}")
            resp.raise_for_status()
            status = resp.json()["status"]
            if as_json:
                console.print_json(data=status)
            else:
                _print_job_status(status)
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to {action} job: {_http_error_detail(e)}") from e


@jobs.command(name="run")
@click.argument("job_id")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_run_cmd(job_id: str, as_json: bool) -> None:
    """Run a queued/future or failed/unavailable job immediately."""
    _post_job_action(job_id, "run", as_json=as_json)


@jobs.command(name="retry")
@click.argument("job_id")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_retry_cmd(job_id: str, as_json: bool) -> None:
    """Retry a failed or unavailable job with a new attempt."""
    _post_job_action(job_id, "retry", as_json=as_json)


@jobs.command(name="cancel")
@click.argument("job_id")
@click.option("--requested-by", "requested_by_peer_id", help="Peer id requesting cancel")
@click.option("--reason", default="cancel_requested", show_default=True)
@click.option("--json", "as_json", is_flag=True, help="Emit JSON status instead of text")
def jobs_cancel_cmd(
    job_id: str,
    requested_by_peer_id: str | None,
    reason: str,
    as_json: bool,
) -> None:
    """Request cancellation for a job."""
    import httpx

    body = {"reason": reason}
    if requested_by_peer_id:
        body["requested_by_peer_id"] = requested_by_peer_id
    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(f"{_get_daemon_url()}/jobs/{job_id}/cancel", json=body)
            if resp.status_code == 404:
                raise click.ClickException(f"No job: {job_id}")
            resp.raise_for_status()
            status = resp.json()["status"]
            if as_json:
                console.print_json(data=status)
            else:
                _print_job_status(status)
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to cancel job: {_http_error_detail(e)}") from e


@jobs.command(name="result")
@click.argument("job_id")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON result")
def jobs_result_cmd(job_id: str, as_json: bool) -> None:
    """Show terminal job result, or current status if not ready."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/jobs/{job_id}/result")
            if resp.status_code == 404:
                raise click.ClickException(f"No job: {job_id}")
            resp.raise_for_status()
            result = resp.json()["result"]
            if as_json:
                console.print_json(data=result)
            elif result.get("result_state") == "not_ready":
                _print_job_status(result.get("status", {}))
            else:
                console.print(f"[green]Job {result.get('job_id') or result.get('work_id')}[/]")
                console.print(f"  state: {result.get('state')}")
                if result.get("summary"):
                    console.print(f"  summary: {result.get('summary')}")
    except httpx.ConnectError:
        raise click.ClickException("Cannot connect to daemon. Run 'repowire serve' first.")
    except httpx.HTTPStatusError as e:
        raise click.ClickException(f"Failed to get job result: {_http_error_detail(e)}") from e


def _print_job_status(status: dict[str, Any], *, created: bool = False) -> None:
    prefix = "Created job" if created else "Job"
    console.print(f"[green]{prefix} {status.get('job_id') or status.get('work_id')}[/]")
    console.print(f"  title: [cyan]{status.get('title') or ''}[/]")
    console.print(f"  state: {status.get('state')}")
    console.print(f"  kind:  {status.get('kind')}")
    if status.get("owner_peer_id"):
        console.print(f"  owner: {status.get('owner_peer_id')}")
    if status.get("assigned_peer_id"):
        console.print(f"  assigned: {status.get('assigned_peer_id')}")
    if status.get("due_at"):
        console.print(f"  due_at: {status.get('due_at')}")
    runner = status.get("runner") or {}
    if runner.get("current_attempt_id"):
        console.print(f"  attempt: {runner.get('current_attempt_id')}")
    if status.get("result_summary"):
        console.print(f"  result: {status.get('result_summary')}")
    if status.get("cancellation_reason"):
        console.print(f"  cancel: {status.get('cancellation_reason')}")


def _print_calendar_status(status: dict[str, Any], *, created: bool = False) -> None:
    prefix = "Created recurring job" if created else "Recurring job"
    console.print(f"[green]{prefix} {status.get('calendar_id') or status.get('recurring_id')}[/]")
    console.print(f"  title: [cyan]{status.get('title') or ''}[/]")
    console.print(f"  state: {status.get('state')}")
    console.print(f"  kind:  {status.get('kind')}")
    console.print(f"  cron:  {status.get('cron')}")
    console.print(f"  next_due_at: {status.get('next_due_at')}")
    if status.get("last_occurrence_work_id"):
        console.print(f"  last_job: {status.get('last_occurrence_work_id')}")


@main.group()
def schedule() -> None:
    """Schedule one-time or recurring mesh messages."""
    pass


@schedule.command(name="self")
@click.argument("when_or_cron")
@click.argument("text")
@click.option(
    "--cron",
    "is_cron",
    is_flag=True,
    help="Treat WHEN_OR_CRON as a recurring cron expression instead of a one-time time.",
)
@click.option("--from-peer", help="Override the calling peer identity")
@click.option("--kind", default="notify", type=click.Choice(["notify", "ask"]))
@click.option("--circle", "-c", default=None, help="Circle to scope self lookup")
def schedule_self_cmd(
    when_or_cron: str,
    text: str,
    is_cron: bool,
    from_peer: str | None,
    kind: str,
    circle: str | None,
) -> None:
    """Schedule a future message to this peer.

    WHEN_OR_CRON may be ISO-8601, a relative time like 10m/1h/"in 30s",
    or a cron expression when --cron is passed.
    """
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            peer_name = from_peer or _current_cli_peer_name(client)
            body = {
                "from_peer": peer_name,
                "to_peer": peer_name,
                "text": text,
                "kind": kind,
            }
            if is_cron:
                body["cron"] = when_or_cron
            else:
                body["fire_at"] = _parse_schedule_when(when_or_cron)
            if circle:
                body["circle"] = circle
            resp = client.post(f"{_get_daemon_url()}/schedules", json=body)
            resp.raise_for_status()
            _print_schedule_created(resp.json())
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
    except httpx.HTTPStatusError as e:
        detail = _http_error_detail(e)
        console.print(f"[red]Failed to create schedule: {detail}[/]")


@schedule.command(name="create")
@click.argument("to_peer")
@click.argument("when_or_cron")
@click.argument("text")
@click.option("--from-peer", required=True, help="Peer identity to send from")
@click.option(
    "--cron",
    "is_cron",
    is_flag=True,
    help="Treat WHEN_OR_CRON as a recurring cron expression instead of a one-time time.",
)
@click.option("--kind", default="notify", type=click.Choice(["notify", "ask"]))
@click.option("--circle", "-c", default=None, help="Circle to scope recipient lookup")
def schedule_create_cmd(
    to_peer: str,
    when_or_cron: str,
    text: str,
    from_peer: str,
    is_cron: bool,
    kind: str,
    circle: str | None,
) -> None:
    """Schedule a future message to another peer."""
    import httpx

    body = {
        "from_peer": from_peer,
        "to_peer": to_peer,
        "text": text,
        "kind": kind,
    }
    if is_cron:
        body["cron"] = when_or_cron
    else:
        body["fire_at"] = _parse_schedule_when(when_or_cron)
    if circle:
        body["circle"] = circle

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.post(f"{_get_daemon_url()}/schedules", json=body)
            resp.raise_for_status()
            _print_schedule_created(resp.json())
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
    except httpx.HTTPStatusError as e:
        detail = _http_error_detail(e)
        console.print(f"[red]Failed to create schedule: {detail}[/]")


@schedule.command(name="list")
@click.option("--from-peer", help="Filter by creator peer")
def schedule_list_cmd(from_peer: str | None) -> None:
    """List pending schedules."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            params = {"from_peer": from_peer} if from_peer else None
            resp = client.get(f"{_get_daemon_url()}/schedules", params=params)
            resp.raise_for_status()
            schedules = resp.json().get("schedules", [])
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        return
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to list schedules: {_http_error_detail(e)}[/]")
        return

    if not schedules:
        console.print("[yellow]No schedules.[/]")
        return
    table = Table(title="Repowire Schedules")
    table.add_column("ID", style="cyan")
    table.add_column("From")
    table.add_column("To")
    table.add_column("Kind")
    table.add_column("Next fire")
    table.add_column("Cron")
    table.add_column("Text")
    for s in schedules:
        table.add_row(
            s.get("schedule_id", ""),
            s.get("from_peer", ""),
            s.get("to_peer", ""),
            s.get("kind", ""),
            s.get("fire_at", ""),
            s.get("cron") or "-",
            (s.get("text") or "").replace("\n", " "),
        )
    console.print(table)


@schedule.command(name="delete")
@click.argument("schedule_id")
def schedule_delete_cmd(schedule_id: str) -> None:
    """Cancel a pending schedule."""
    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resp = client.delete(f"{_get_daemon_url()}/schedules/{schedule_id}")
            if resp.status_code == 404:
                console.print(f"[red]No schedule: {schedule_id}[/]")
                return
            resp.raise_for_status()
            console.print(f"[green]Deleted schedule {schedule_id}[/]")
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to delete schedule: {_http_error_detail(e)}[/]")


def _print_schedule_created(schedule: dict) -> None:
    console.print(f"[green]Scheduled {schedule.get('schedule_id')}[/]")
    console.print(f"  from: [cyan]{schedule.get('from_peer')}[/]")
    console.print(f"  to:   [cyan]{schedule.get('to_peer')}[/]")
    console.print(f"  kind: {schedule.get('kind')}")
    console.print(f"  next: {schedule.get('fire_at')}")
    if schedule.get("cron"):
        console.print(f"  cron: {schedule.get('cron')}")


def _http_error_detail(error: object) -> str:
    response = getattr(error, "response", None)
    if response is None:
        return str(error)
    return _http_response_detail(response)


def _http_response_detail(response: Any) -> str:
    try:
        body = response.json()
    except Exception:
        return str(response)
    return str(body.get("detail") or body)


@peer.command(name="list")
@click.option("--show-offline", "-a", is_flag=True, help="Include offline peers")
def peer_list(show_offline: bool) -> None:
    """List all registered peers and their status."""
    import httpx

    try:
        params = None if show_offline else {"status": "online"}
        with httpx.Client(timeout=5.0) as client:
            resp = client.get(f"{_get_daemon_url()}/peers", params=params)
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
    table.add_column("Backend")
    table.add_column("Circle", style="magenta")
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
            p.get("backend") or "-",
            p.get("circle") or "global",
            p.get("path") or "-",
        )

    console.print(table)


@peer.command(name="describe")
@click.argument("identifier")
@click.option(
    "--circle",
    "-c",
    default=None,
    help="Circle to scope lookup when a display_name is ambiguous across circles.",
)
def peer_describe(identifier: str, circle: str | None) -> None:
    """Show all known state for one peer.

    IDENTIFIER may be a display_name (e.g. 'clitcoin-claude-code') or a
    peer_id (e.g. 'repow-5-abd4d21e'). When a display_name matches multiple
    peers across circles, pass --circle to disambiguate.
    """
    import sys

    import httpx

    from repowire.peer_describe import (
        ResolveAmbiguous,
        ResolveNotFound,
        ambiguity_message,
        build_snapshot,
        fetch_all_peers,
        resolve_peer,
    )

    daemon_url = _get_daemon_url()
    try:
        with httpx.Client(timeout=5.0) as client:
            peers = fetch_all_peers(client, daemon_url)
            outcome = resolve_peer(peers, identifier, circle=circle)
            if isinstance(outcome, ResolveNotFound):
                console.print(f"[red]Peer not found:[/] {identifier}")
                console.print("[dim]Run `repowire peer list` to see available names.[/]")
                sys.exit(1)
            if isinstance(outcome, ResolveAmbiguous):
                console.print(f"[red]{ambiguity_message(outcome)}[/]")
                sys.exit(1)
            snapshot = build_snapshot(client, daemon_url, outcome)
    except httpx.ConnectError:
        console.print(
            f"[red]Cannot connect to daemon at {daemon_url}.[/] Run 'repowire serve' first.",
        )
        sys.exit(1)
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Daemon error: {e}[/]")
        sys.exit(1)

    _render_peer_snapshot(snapshot)


@peer.command(name="claim-role")
@click.argument("role", type=click.Choice(["orchestrator"]))
@click.option("--peer", "peer_name", help="Existing peer id or display name to update")
@click.option("--circle", "-c", help="Circle to scope lookup and role ownership")
@click.option("--force", "-f", is_flag=True, help="Retry repair with compatibility force flag")
def peer_claim_role(role: str, peer_name: str | None, circle: str | None, force: bool) -> None:
    """Claim a singleton special role for an existing peer."""
    import sys

    import httpx

    daemon_url = _get_daemon_url()
    try:
        with httpx.Client(timeout=5.0) as client:
            target = peer_name or _current_cli_peer_name(client)
            resp = client.post(
                f"{daemon_url}/peers/claim-role",
                json={
                    "role": role,
                    "peer_name": target,
                    "circle": circle,
                    "force": force,
                },
            )
            if resp.status_code == 404:
                console.print(f"[red]Peer not found:[/] {target}")
                console.print(
                    "[dim]Pass --peer with an existing peer from `repowire peer list -a`.[/]"
                )
                sys.exit(1)
            if resp.status_code == 409:
                console.print(f"[red]Cannot claim role:[/] {_http_response_detail(resp)}")
                console.print(
                    "[dim]A fresh live orchestrator holder cannot be demoted; "
                    "stop it first, then retry.[/]"
                )
                sys.exit(1)
            resp.raise_for_status()
            data = resp.json()
    except httpx.ConnectError:
        console.print(
            f"[red]Cannot connect to daemon at {daemon_url}.[/] Run 'repowire serve' first.",
        )
        sys.exit(1)
    except httpx.HTTPStatusError as e:
        console.print(f"[red]Failed to claim role:[/] {_http_error_detail(e)}")
        sys.exit(1)

    console.print(
        f"[green]Claimed role={data.get('role')}[/] for "
        f"[cyan]{data.get('peer_name')}[/] in circle [magenta]{data.get('circle')}[/]"
    )
    holders = data.get("previous_holders") or []
    if holders:
        console.print(f"[dim]Demoted {len(holders)} previous holder(s).[/]")


def _render_peer_snapshot(snapshot: object) -> None:
    """Render a PeerSnapshot to the console."""
    from repowire.peer_describe import PeerSnapshot, humanize_last_seen

    assert isinstance(snapshot, PeerSnapshot)
    p = snapshot.peer
    status = p.get("status", "?")
    status_color = {"online": "green", "busy": "yellow", "offline": "red"}.get(status, "white")

    rows: list[tuple[str, str]] = [
        ("Peer", f"[cyan]{p.get('display_name', '?')}[/]  [dim]({p.get('peer_id', '?')})[/]"),
        ("Project", p.get("metadata", {}).get("project") or "[dim]-[/]"),
        ("Circle", f"[magenta]{p.get('circle', 'global')}[/]"),
        ("Role", p.get("role", "agent")),
        ("Backend", p.get("backend", "?")),
        ("Status", f"[{status_color}]{status}[/]"),
        ("Path", p.get("path") or "[dim]-[/]"),
        ("Machine", p.get("machine") or "[dim]-[/]"),
        ("Last seen", humanize_last_seen(p.get("last_seen"))),
        ("Turn state", p.get("turn_state") or "[dim]-[/]"),
        ("Description", p.get("description") or "[dim]-[/]"),
    ]

    for label, value in rows:
        console.print(f"[bold]{label + ':':14}[/] {value}")

    console.print("")
    n_in = len(snapshot.inbound_asks)
    n_out = len(snapshot.outbound_asks)
    console.print(f"[bold]Open asks:[/]    {n_in} inbound, {n_out} outbound")
    for a in snapshot.inbound_asks:
        when = humanize_last_seen(a.created_at)
        snippet = a.text if len(a.text) <= 60 else a.text[:57] + "..."
        console.print(
            f"  [green]inbound[/]   {a.correlation_id}  from [cyan]@{a.from_peer}[/]  "
            f'{when}  [dim]"{snippet}"[/]',
        )
    for a in snapshot.outbound_asks:
        when = humanize_last_seen(a.created_at)
        snippet = a.text if len(a.text) <= 60 else a.text[:57] + "..."
        console.print(
            f"  [yellow]outbound[/]  {a.correlation_id}  to [cyan]@{a.to_peer}[/]  "
            f'{when}  [dim]"{snippet}"[/]',
        )

    console.print("")
    console.print(f"[bold]Recent activity:[/]  (last {len(snapshot.recent_activity)} events)")
    if not snapshot.recent_activity:
        console.print("  [dim]no recent events[/]")
        return
    for ev in snapshot.recent_activity:
        ts = ev.timestamp[11:16] if len(ev.timestamp) >= 16 else ev.timestamp
        arrow = {"inbound": "←", "outbound": "→", "self": "·"}.get(ev.direction, " ")
        cp = f"@{ev.counterparty}" if ev.counterparty else ""
        snippet = ev.text if len(ev.text) <= 60 else ev.text[:57] + "..."
        line = f"  [dim]{ts}[/]  {arrow} [cyan]{ev.event_type}[/] {cp}"
        if snippet:
            line += f'  [dim]"{snippet}"[/]'
        console.print(line)


@peer.command(name="new")
@click.argument("path", type=click.Path(exists=True), default=".")
@click.option(
    "--backend",
    "-b",
    default="claude-code",
    type=click.Choice(["claude-code", "opencode", "codex", "gemini", "antigravity", "pi"]),
)
@click.option("--command", "-c", "cmd", help="Deprecated: explicit command override")
@click.option("--profile", help="Named spawn profile to apply to the backend command")
@click.option("--circle", help="Circle (defaults to 'default')")
def peer_new(
    path: str,
    backend: str,
    cmd: str | None,
    profile: str | None,
    circle: str | None,
) -> None:  # noqa: ARG001
    """Spawn a new peer in a tmux window.

    Examples:

        repowire peer new ~/git/myproject

        repowire peer new . --backend=claude-code

        repowire peer new ~/git/api --backend=opencode --circle=backend
    """
    from repowire.config.models import AgentType
    from repowire.spawn import SpawnConfig, spawn_peer

    actual_path = str(Path(path).resolve())
    actual_circle = circle or "default"
    actual_cmd = cmd or _configured_spawn_command(backend, profile)
    if not actual_cmd:
        if profile:
            console.print(
                f"[red]No daemon.spawn.profiles entry for {backend!r}/{profile!r}.[/] "
                "Configure it in ~/.repowire/config.yaml or omit --profile."
            )
        else:
            console.print(
                f"[red]No daemon.spawn.commands entry for {backend!r}.[/] "
                "Configure it in ~/.repowire/config.yaml or pass --command."
            )
        return
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
        if profile and not cmd:
            console.print(f"  profile: {profile}")
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


@peer.command(name="restart")
@click.argument("name")
@click.option("--circle", "-c", help="Circle to scope display-name lookup")
@click.option("--from-peer", help="Caller peer identity for future authorization")
@click.option("--dry-run", is_flag=True, help="Check restart capability without killing")
@click.option("--message", "-m", help="Optional seed message for the restarted runtime")
def peer_restart(
    name: str,
    circle: str | None,
    from_peer: str | None,
    dry_run: bool,
    message: str | None,
) -> None:
    """Restart a daemon-spawned peer while preserving mesh identity."""
    from urllib.parse import quote

    import httpx

    body: dict[str, object] = {"dry_run": dry_run}
    if circle:
        body["circle"] = circle
    if from_peer:
        body["from_peer"] = from_peer
    if message:
        body["message"] = message

    try:
        with httpx.Client(timeout=30.0) as client:
            resp = client.post(
                f"{_get_daemon_url()}/peers/{quote(name, safe='')}/restart",
                json=body,
            )
            if resp.status_code == 404:
                raise click.ClickException(f"Peer '{name}' not found")
            resp.raise_for_status()
            data = resp.json()
    except httpx.ConnectError:
        raise click.ClickException(
            "Cannot connect to daemon. Run 'repowire serve' first."
        ) from None
    except httpx.HTTPStatusError as e:
        detail = e.response.text
        try:
            detail = e.response.json().get("detail", detail)
        except ValueError:
            pass
        raise click.ClickException(f"Failed to restart peer: {detail}") from e

    action = "Restart available" if dry_run else "Restarted"
    console.print(
        f"[green]✓[/] {action} for [cyan]{data.get('display_name')}[/] "
        f"([dim]{data.get('peer_id')}[/])"
    )
    console.print(f"  backend: {data.get('backend')}")
    mode = data.get("resume_mode")
    mode_label = "[green]resumed[/]" if mode == "resumed" else mode
    console.print(f"  mode: {mode_label}")
    if data.get("resume_warning"):
        console.print(f"  [yellow]context not resumed:[/] {data.get('resume_warning')}")
    if data.get("tmux_session"):
        console.print(f"  tmux: {data.get('tmux_session')}")


@peer.command(name="doctor")
@click.argument("name")
@click.option("--circle", "-c", help="Circle to scope display-name lookup")
@click.option("--json", "json_out", is_flag=True, help="Emit the raw report as JSON")
@click.option(
    "--fix",
    is_flag=True,
    help="Attempt a non-destructive rehook when an inbound-down contradiction is found",
)
def peer_doctor(name: str, circle: str | None, json_out: bool, fix: bool) -> None:
    """Deep diagnostic for a peer; exits nonzero on error-severity contradictions."""
    import json as _json
    from urllib.parse import quote

    data = _daemon_request_json(
        "GET",
        f"/peers/{quote(name, safe='')}/doctor",
        params={"circle": circle} if circle else None,
        not_found_msg=f"Peer '{name}' not found",
        error_prefix="Failed to run doctor",
    )

    if json_out:
        click.echo(_json.dumps(data, indent=2))
    else:
        _render_doctor_report(data)

    codes = {c.get("code") for c in data.get("contradictions", [])}
    if fix and ({"ONLINE_BUT_NO_WS", "PANE_MISSING"} & codes):
        console.print("\n[dim]--fix: attempting non-destructive rehook...[/]")
        _rehook_peer(name, circle=circle, apply=True)

    if any(c.get("severity") == "error" for c in data.get("contradictions", [])):
        raise SystemExit(1)


def _rehook_peer(name: str, *, circle: str | None, apply: bool) -> None:
    """Call the rehook endpoint and print the outcome. Shared by `peer rehook` + doctor --fix."""
    from urllib.parse import quote

    body: dict[str, object] = {"apply": apply}
    if circle:
        body["circle"] = circle
    data = _daemon_request_json(
        "POST",
        f"/peers/{quote(name, safe='')}/rehook",
        json_body=body,
        not_found_msg=f"Peer '{name}' not found",
        error_prefix="Failed to rehook",
        timeout=20.0,
    )

    acted = data.get("acted")
    mark = "[green]✓[/]" if acted else "[yellow]·[/]"
    console.print(
        f"  {mark} rehook {data.get('display_name')}: "
        f"reason={data.get('reason')} "
        f"respawned={data.get('ws_hook_respawned')} "
        f"acted={acted}"
    )


def _render_doctor_report(d: dict) -> None:
    """Pretty-print a DoctorReport dict."""
    console.print(
        f"\n[bold]peer doctor:[/] [cyan]{d.get('display_name')}[/] "
        f"([dim]{d.get('peer_id')}[/])\n"
    )
    console.print("[bold]Identity[/]")
    console.print(f"  status      {d.get('status')}    turn_state  {d.get('turn_state') or '-'}")
    console.print(f"  backend     {d.get('backend')}    circle      {d.get('circle')}")
    console.print(f"  machine     {d.get('machine')}    role        {d.get('role')}")
    console.print(f"  last_seen   {d.get('last_seen') or '-'}")
    if d.get("pane_id"):
        console.print(f"  pane_id     {d.get('pane_id')}")

    locality = (
        "local machine"
        if d.get("is_local_machine")
        else "remote machine - local probes unavailable"
    )
    console.print(f"\n[bold]Inbound reachability[/]   [dim]({locality})[/]")
    console.print(f"  ws_connected      {'yes' if d.get('ws_connected') else 'no'}")
    pane_exists = d.get("tmux_pane_exists")
    pane_str = "unavailable" if pane_exists is None else ("yes" if pane_exists else "no")
    ev = d.get("tmux_evidence")
    if ev:
        pane_str += f"   ({d.get('pane_id')} → {ev.get('tmux_session')}, pid {ev.get('pane_pid')})"
    console.print(f"  tmux_pane_exists  {pane_str}")
    if d.get("hook_meta_available"):
        console.print(
            f"  hook_meta         peer_id={d.get('hook_meta_peer_id')} "
            f"name={d.get('hook_meta_display_name')}"
        )
    else:
        console.print("  hook_meta         unavailable")
    pid_alive = d.get("agent_pid_alive")
    pid_str = "unavailable" if pid_alive is None else ("alive" if pid_alive else "DEAD")
    console.print(f"  agent_pid         {d.get('agent_pid') or '-'}  ({pid_str})")

    console.print("\n[bold]Asks[/]")
    age = d.get("oldest_pending_age_seconds")
    age_str = f"  oldest {int(age // 60)}m ago ({d.get('oldest_pending_cid')})" if age else ""
    console.print(f"  pending inbound   {d.get('pending_inbound_count', 0)}{age_str}")

    contradictions = d.get("contradictions", [])
    if not contradictions:
        console.print("\n[green]✓ No contradictions detected[/]")
        return
    console.print(f"\n[bold]Contradictions ({len(contradictions)})[/]")
    for c in contradictions:
        mark = "[red]✗[/]" if c.get("severity") == "error" else "[yellow]●[/]"
        console.print(f"  {mark} {c.get('code')}   {c.get('detail')}")


@main.command(name="trace")
@click.argument("trace_id")
@click.option("--json", "json_out", is_flag=True, help="Emit the raw trace as JSON")
def trace_cmd(trace_id: str, json_out: bool) -> None:
    """Show the delivery stages for an ask (correlation_id) or notify (delivery_id)."""
    import json as _json
    from urllib.parse import quote

    data = _daemon_request_json(
        "GET",
        f"/traces/{quote(trace_id, safe='')}",
        not_found_msg=f"No delivery trace for '{trace_id}'",
        error_prefix="Failed to fetch trace",
        timeout=10.0,
    )

    if json_out:
        click.echo(_json.dumps(data, indent=2))
    else:
        console.print(
            f"\n[bold]trace:[/] [cyan]{data.get('trace_id')}[/] "
            f"([dim]{data.get('kind')}[/])\n"
        )
        for s in data.get("stages", []):
            mark = "[red]✗[/]" if s.get("status") == "fail" else "[green]·[/]"
            peer_str = f"  → {s.get('peer_id')}" if s.get("peer_id") else ""
            stage_name = s.get("stage", "")
            console.print(
                f"  {mark} [{s.get('seq')}] {stage_name:<16} {s.get('ts')}{peer_str}"
            )

    if any(s.get("status") == "fail" for s in data.get("stages", [])):
        raise SystemExit(1)


@peer.command(name="rehook")
@click.argument("name")
@click.option("--circle", "-c", help="Circle to scope display-name lookup")
@click.option("--apply", is_flag=True, help="Act instead of dry-run (default is dry-run)")
def peer_rehook(name: str, circle: str | None, apply: bool) -> None:
    """Re-establish a peer's inbound ws-hook without killing the pane.

    Defaults to a dry-run report; pass --apply to act. A ping-healthy peer is
    never disconnected. This is non-destructive — for a destructive restart use
    `repowire peer restart`.
    """
    if not apply:
        console.print("[dim](dry-run — pass --apply to act)[/]")
    _rehook_peer(name, circle=circle, apply=apply)


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


def _auth_headers() -> dict[str, str]:
    """Return Authorization headers from daemon.auth_token if configured.

    Reads `~/.repowire/config.yaml` via `load_config()`. Returns an empty dict
    when no token is set (so callers can always splat `**_auth_headers()`).
    Swallows config-load errors silently — the daemon will return 401 if auth
    is actually required, which is a clearer signal than a CLI-side traceback.
    """
    try:
        from repowire.config.models import load_config

        token = load_config().daemon.auth_token
    except Exception:
        return {}
    return {"Authorization": f"Bearer {token}"} if token else {}


def _daemon_request_json(
    method: str,
    path: str,
    *,
    not_found_msg: str,
    error_prefix: str,
    json_body: dict | None = None,
    params: dict | None = None,
    timeout: float = 15.0,
) -> dict:
    """Call a daemon endpoint and return parsed JSON, raising ClickException on failure.

    Centralizes the connect-error / 404 / HTTPStatusError-detail handling shared
    by the diagnostic CLI commands (doctor, trace, rehook).
    """
    import httpx

    try:
        with httpx.Client(timeout=timeout) as client:
            resp = client.request(
                method,
                f"{_get_daemon_url()}{path}",
                json=json_body,
                params=params,
                headers={**_auth_headers()},
            )
            if resp.status_code == 404:
                raise click.ClickException(not_found_msg)
            resp.raise_for_status()
            return resp.json()
    except httpx.ConnectError:
        raise click.ClickException(
            "Cannot connect to daemon. Run 'repowire serve' first."
        ) from None
    except httpx.HTTPStatusError as e:
        detail = e.response.text
        try:
            detail = e.response.json().get("detail", detail)
        except ValueError:
            pass
        raise click.ClickException(f"{error_prefix}: {detail}") from e


def _resolve_peer_id_for_asks(
    client: Any,
    *,
    peer_id: str | None,
    pane_id: str | None,
    peer_name: str | None,
) -> tuple[str | None, str | None]:
    """Resolve a peer_id for /asks/pending lookup.

    Order: explicit peer_id → explicit pane_id → TMUX_PANE → --peer NAME.
    Returns (peer_id, error_message). On success error_message is None.
    """
    import os
    from urllib.parse import quote

    daemon = _get_daemon_url()
    headers = _auth_headers()

    if peer_id:
        return peer_id, None

    pane = pane_id or os.environ.get("TMUX_PANE")
    if pane:
        resp = client.get(f"{daemon}/peers/by-pane/{quote(pane, safe='')}", headers=headers)
        if resp.status_code == 200:
            body = resp.json()
            resolved = body.get("peer_id")
            if resolved:
                return resolved, None
        if not peer_name:
            return None, (
                f"No peer registered for pane {pane}. "
                "Run `repowire peer whoami --register --backend <type>` first."
            )

    if peer_name:
        resp = client.get(f"{daemon}/peers/{quote(peer_name, safe='')}", headers=headers)
        if resp.status_code == 404:
            return None, f"Peer '{peer_name}' not found"
        resp.raise_for_status()
        return resp.json().get("peer_id"), None

    return None, "No peer identity (set TMUX_PANE, or pass --peer-id/--pane-id/--peer)"


@peer.command(name="whoami")
@click.option("--register", is_flag=True, help="Register a new peer instead of read-only lookup")
@click.option("--name", help="Display name (defaults to current folder name)")
@click.option(
    "--backend",
    type=click.Choice(["claude-code", "codex", "gemini", "antigravity", "opencode", "pi"]),
    help="Agent backend type (required with --register)",
)
@click.option("--circle", "-c", default=None, help="Circle (default: 'default')")
@click.option("--path", "-p", default=None, help="Working directory (defaults to cwd)")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON instead of formatted text")
def peer_whoami(
    register: bool,
    name: str | None,
    backend: str | None,
    circle: str | None,
    path: str | None,
    as_json: bool,
) -> None:
    """Print current peer identity, or register a new peer with --register.

    Read-only mode (default): uses TMUX_PANE → /peers/by-pane, or --name → /peers/{name}.
    Register mode: POSTs /peers with backend/circle/path (and TMUX_PANE if present).
    Designed as the bootstrap entry point for agents whose hooks don't fire (e.g. agy).
    """
    import json as _json
    import os
    import sys
    from urllib.parse import quote

    import httpx

    daemon = _get_daemon_url()
    headers = _auth_headers()
    pane_id = os.environ.get("TMUX_PANE")

    def _emit(info: dict) -> None:
        if as_json:
            click.echo(_json.dumps(info, indent=2))
        else:
            console.print(f"[cyan]peer_id:[/] {info.get('peer_id', '')}")
            display = info.get("display_name") or info.get("name", "")
            console.print(f"[cyan]display_name:[/] {display}")
            if info.get("backend"):
                console.print(f"[cyan]backend:[/] {info['backend']}")
            if info.get("circle"):
                console.print(f"[cyan]circle:[/] {info['circle']}")
            if info.get("status"):
                console.print(f"[cyan]status:[/] {info['status']}")

    try:
        with httpx.Client(timeout=5.0) as client:
            if register:
                if not backend:
                    console.print("[red]--register requires --backend[/]")
                    sys.exit(2)
                actual_path = path or str(Path.cwd())
                actual_name = name or Path(actual_path).name
                body: dict = {
                    "name": actual_name,
                    "path": actual_path,
                    "backend": backend,
                    "metadata": {"repowire_cli_fallback": True},
                }
                if circle:
                    body["circle"] = circle
                if pane_id:
                    body["pane_id"] = pane_id
                resp = client.post(f"{daemon}/peers", json=body, headers=headers)
                if resp.status_code >= 400:
                    console.print(f"[red]Register failed: {resp.status_code} {resp.text}[/]")
                    sys.exit(1)
                data = resp.json()
                _emit(
                    {
                        "peer_id": data.get("peer_id"),
                        "display_name": data.get("display_name"),
                        "backend": backend,
                        "circle": circle or "default",
                    }
                )
                return

            # Read-only lookup
            if pane_id and not name:
                resp = client.get(
                    f"{daemon}/peers/by-pane/{quote(pane_id, safe='')}",
                    headers=headers,
                )
                if resp.status_code == 200:
                    _emit(resp.json())
                    return
            if name:
                resp = client.get(f"{daemon}/peers/{quote(name, safe='')}", headers=headers)
                if resp.status_code == 404:
                    console.print(f"[red]Peer '{name}' not found[/]")
                    sys.exit(1)
                resp.raise_for_status()
                _emit(resp.json())
                return
            console.print(
                "[yellow]No registered peer for this pane.[/] "
                "Pass --name to look up by name, or --register to create one."
            )
            sys.exit(1)
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        sys.exit(1)


@peer.command(name="asks")
@click.option("--peer-id", "peer_id", default=None, help="Explicit peer_id to query")
@click.option("--pane-id", "pane_id", default=None, help="Tmux pane id (overrides $TMUX_PANE)")
@click.option("--peer", "peer_name", default=None, help="Resolve peer_id via display name")
@click.option(
    "--direction",
    type=click.Choice(["inbound", "outbound", "both"]),
    default="inbound",
)
@click.option("--json", "as_json", is_flag=True, help="Emit JSON instead of table")
def peer_asks(
    peer_id: str | None,
    pane_id: str | None,
    peer_name: str | None,
    direction: str,
    as_json: bool,
) -> None:
    """List pending asks for this peer (CLI fallback for agents without hooks)."""
    import json as _json
    import sys

    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resolved, err = _resolve_peer_id_for_asks(
                client,
                peer_id=peer_id,
                pane_id=pane_id,
                peer_name=peer_name,
            )
            if err:
                console.print(f"[red]{err}[/]")
                sys.exit(1)

            resp = client.get(
                f"{_get_daemon_url()}/asks/pending",
                params={"peer_id": resolved, "direction": direction},
                headers=_auth_headers(),
            )
            if resp.status_code >= 400:
                console.print(f"[red]/asks/pending failed: {resp.status_code} {resp.text}[/]")
                sys.exit(1)
            asks = resp.json().get("asks", [])
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        sys.exit(1)

    if as_json:
        click.echo(_json.dumps(asks, indent=2))
        return

    if not asks:
        console.print("[dim]No pending asks[/]")
        return

    for a in asks:
        cid = a.get("correlation_id", "")
        frm = a.get("from_peer", "?")
        to = a.get("to_peer", "?")
        text = (a.get("text") or "").replace("\n", " ")
        if len(text) > 100:
            text = text[:97] + "..."
        console.print(f"[cyan]{cid}[/]  {frm} → {to}  {text}")


@peer.command(name="deliveries")
@click.option("--peer-id", "peer_id", default=None, help="Explicit peer_id to drain")
@click.option("--pane-id", "pane_id", default=None, help="Tmux pane id (overrides $TMUX_PANE)")
@click.option("--peer", "peer_name", default=None, help="Resolve peer_id via display name")
@click.option("--json", "as_json", is_flag=True, help="Emit JSON instead of formatted text")
def peer_deliveries(
    peer_id: str | None,
    pane_id: str | None,
    peer_name: str | None,
    as_json: bool,
) -> None:
    """Drain queued deliveries for this peer (CLI fallback for polling agents)."""
    import json as _json
    import sys

    import httpx

    try:
        with httpx.Client(timeout=5.0) as client:
            resolved, err = _resolve_peer_id_for_asks(
                client,
                peer_id=peer_id,
                pane_id=pane_id,
                peer_name=peer_name,
            )
            if err:
                console.print(f"[red]{err}[/]")
                sys.exit(1)
            resp = client.get(
                f"{_get_daemon_url()}/deliveries/pending",
                params={"peer_id": resolved},
                headers=_auth_headers(),
            )
            if resp.status_code >= 400:
                console.print(f"[red]/deliveries/pending failed: {resp.status_code} {resp.text}[/]")
                sys.exit(1)
            deliveries = resp.json().get("deliveries", [])
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        sys.exit(1)

    if as_json:
        click.echo(_json.dumps(deliveries, indent=2))
        return

    if not deliveries:
        console.print("[dim]No queued deliveries[/]")
        return

    for delivery in deliveries:
        kind = delivery.get("kind", "notify")
        cid = delivery.get("correlation_id")
        frm = delivery.get("from_peer", "?")
        text = (delivery.get("text") or "").replace("\n", " ")
        if len(text) > 120:
            text = text[:117] + "..."
        label = f"{kind} #{cid}" if cid else kind
        console.print(f"[cyan]{label}[/]  from {frm}  {text}")


@peer.command(name="ack")
@click.argument("correlation_id")
@click.option("-m", "--message", default=None, help="Optional reply message (closes the thread)")
@click.option("--from-peer", "from_peer", default=None, help="Acking peer identity (compat-only)")
def peer_ack(correlation_id: str, message: str | None, from_peer: str | None) -> None:
    """Close an open ask. With -m, delivers the message as a reply."""
    import sys

    import httpx

    body: dict = {"correlation_id": correlation_id}
    if message is not None:
        body["message"] = message
    if from_peer:
        body["from_peer"] = from_peer

    try:
        with httpx.Client(timeout=10.0) as client:
            resp = client.post(f"{_get_daemon_url()}/ack", json=body, headers=_auth_headers())
    except httpx.ConnectError:
        console.print("[red]Cannot connect to daemon. Run 'repowire serve' first.[/]")
        sys.exit(1)

    if resp.status_code == 200:
        console.print(f"[green]Acked #{correlation_id}{' with reply' if message else ''}[/]")
        return
    if resp.status_code == 404:
        console.print(f"[red]No open ask with correlation_id: {correlation_id}[/]")
        sys.exit(1)
    if resp.status_code == 410:
        console.print(f"[red]Ask {correlation_id} is already closed; reply not delivered[/]")
        sys.exit(1)
    if resp.status_code == 503:
        console.print("[red]Reply delivery failed (asker has no live transport); ask still open[/]")
        sys.exit(1)
    console.print(f"[red]/ack failed: {resp.status_code} {resp.text}[/]")
    sys.exit(1)


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
# slack command group
# =============================================================================


@main.group(hidden=True)
def slack() -> None:
    """Manage the Slack bot peer."""
    pass


@slack.command(name="start")
def slack_start() -> None:
    """Start the Slack bot as a repowire peer."""
    from repowire.slack.bot import main as bot_main

    bot_main()


# =============================================================================
# service command group - system service management
# =============================================================================


@main.group()
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
    """Internal hook handlers (called by Claude Code or Codex)."""
    pass


@hook.command(name="stop")
@click.option("--backend", default="claude-code", help="Agent backend")
def hook_stop(backend: str) -> None:
    """Handle Stop hook - capture response for pending queries."""
    import sys

    from repowire.hooks.stop_handler import main as stop_main

    sys.exit(stop_main(backend=backend))


@hook.command(name="session")
@click.option("--backend", default="claude-code", help="Agent backend")
def hook_session(backend: str) -> None:
    """Handle SessionStart/SessionEnd hooks - auto-register/unregister peers."""
    import sys

    from repowire.hooks.session_handler import main as session_main

    sys.exit(session_main(backend=backend))


@hook.command(name="prompt")
@click.option("--backend", default="claude-code", help="Agent backend")
def hook_prompt(backend: str) -> None:
    """Handle UserPromptSubmit hook - mark peer as busy."""
    import sys

    from repowire.hooks.prompt_handler import main as prompt_main

    sys.exit(prompt_main(backend=backend))


@hook.command(name="notification")
def hook_notification() -> None:
    """Handle Notification hook - mark peer as online on idle."""
    import sys

    from repowire.hooks.notification_handler import main as notification_main

    sys.exit(notification_main())


@hook.command(name="pretooluse")
@click.option("--backend", default="claude-code", help="Agent backend")
def hook_pretooluse(backend: str) -> None:
    """Handle PreToolUse hook - remote tool approval when opted in."""
    import sys

    from repowire.hooks.pretooluse_handler import main as pretooluse_main

    sys.exit(pretooluse_main(backend=backend))


if __name__ == "__main__":
    main()
