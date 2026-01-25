"""Platform-specific service installation for repowire daemon."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Literal

# Service identifiers
MACOS_LABEL = "io.repowire.daemon"
LINUX_SERVICE_NAME = "repowire"


def get_platform() -> Literal["macos", "linux", "unsupported"]:
    """Detect the current platform."""
    if sys.platform == "darwin":
        return "macos"
    elif sys.platform.startswith("linux"):
        return "linux"
    return "unsupported"


def _get_repowire_executable() -> str:
    """Get the path to the repowire executable."""
    # Try to find repowire in PATH
    repowire_path = shutil.which("repowire")
    if repowire_path:
        return repowire_path

    # Fallback to uvx for pip-installed scenarios
    uvx_path = shutil.which("uvx")
    if uvx_path:
        return f"{uvx_path} repowire"

    # Last resort: use python -m
    return f"{sys.executable} -m repowire.cli"


def _get_log_path() -> Path:
    """Get the path for daemon logs."""
    log_dir = Path.home() / ".repowire"
    log_dir.mkdir(parents=True, exist_ok=True)
    return log_dir / "daemon.log"


# =============================================================================
# macOS launchd
# =============================================================================


def _get_launchd_plist_path() -> Path:
    """Get the path to the launchd plist file."""
    return Path.home() / "Library" / "LaunchAgents" / f"{MACOS_LABEL}.plist"


def _generate_launchd_plist() -> str:
    """Generate the launchd plist content."""
    repowire_exec = _get_repowire_executable()
    log_path = _get_log_path()

    # Build program arguments
    if " " in repowire_exec:
        # Handle cases like "uvx repowire" or "python -m repowire.cli"
        parts = repowire_exec.split()
        program_args = "".join(f"        <string>{p}</string>\n" for p in parts)
    else:
        program_args = f"        <string>{repowire_exec}</string>\n"

    program_args += "        <string>serve</string>\n"

    # Get current PATH so launchd can find tmux and other tools
    current_path = os.environ.get("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

    return f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{MACOS_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
{program_args.rstrip()}
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{current_path}</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{log_path}</string>
    <key>StandardErrorPath</key>
    <string>{log_path}</string>
</dict>
</plist>
"""


def _install_macos_service() -> tuple[bool, str]:
    """Install launchd service on macOS."""
    plist_path = _get_launchd_plist_path()

    # Ensure LaunchAgents directory exists
    plist_path.parent.mkdir(parents=True, exist_ok=True)

    # Unload existing service if present
    if plist_path.exists():
        subprocess.run(
            ["launchctl", "unload", str(plist_path)],
            capture_output=True,
        )

    # Write plist file
    plist_content = _generate_launchd_plist()
    plist_path.write_text(plist_content)

    # Load the service
    result = subprocess.run(
        ["launchctl", "load", str(plist_path)],
        capture_output=True,
        text=True,
    )

    if result.returncode != 0:
        return False, f"Failed to load service: {result.stderr}"

    return True, f"Service installed at {plist_path}"


def _uninstall_macos_service() -> tuple[bool, str]:
    """Uninstall launchd service on macOS."""
    plist_path = _get_launchd_plist_path()

    if not plist_path.exists():
        return False, "Service is not installed"

    # Unload the service
    result = subprocess.run(
        ["launchctl", "unload", str(plist_path)],
        capture_output=True,
        text=True,
    )

    # Remove plist file
    plist_path.unlink()

    if result.returncode != 0:
        return True, "Service removed (was not running)"

    return True, "Service stopped and removed"


def _get_macos_service_status() -> dict:
    """Get launchd service status on macOS."""
    plist_path = _get_launchd_plist_path()

    if not plist_path.exists():
        return {"installed": False, "running": False, "path": str(plist_path)}

    # Check if service is running
    result = subprocess.run(
        ["launchctl", "list", MACOS_LABEL],
        capture_output=True,
        text=True,
    )

    running = result.returncode == 0
    pid = None
    if running and result.stdout:
        # Parse output: PID\tStatus\tLabel
        parts = result.stdout.strip().split("\t")
        if len(parts) >= 1 and parts[0] != "-":
            try:
                pid = int(parts[0])
            except ValueError:
                pass

    return {
        "installed": True,
        "running": running,
        "pid": pid,
        "path": str(plist_path),
    }


# =============================================================================
# Linux systemd
# =============================================================================


def _get_systemd_service_path() -> Path:
    """Get the path to the systemd user service file."""
    return Path.home() / ".config" / "systemd" / "user" / f"{LINUX_SERVICE_NAME}.service"


def _generate_systemd_unit() -> str:
    """Generate the systemd unit file content."""
    repowire_exec = _get_repowire_executable()
    log_path = _get_log_path()
    current_path = os.environ.get("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

    exec_start = f"{repowire_exec} serve"

    return f"""[Unit]
Description=Repowire Daemon
After=network.target

[Service]
Type=simple
ExecStart={exec_start}
Environment=PATH={current_path}
Restart=always
RestartSec=5
StandardOutput=append:{log_path}
StandardError=append:{log_path}

[Install]
WantedBy=default.target
"""


def _install_linux_service() -> tuple[bool, str]:
    """Install systemd user service on Linux."""
    service_path = _get_systemd_service_path()

    # Ensure systemd user directory exists
    service_path.parent.mkdir(parents=True, exist_ok=True)

    # Stop existing service if running
    subprocess.run(
        ["systemctl", "--user", "stop", LINUX_SERVICE_NAME],
        capture_output=True,
    )

    # Write service file
    unit_content = _generate_systemd_unit()
    service_path.write_text(unit_content)

    # Reload systemd
    subprocess.run(
        ["systemctl", "--user", "daemon-reload"],
        capture_output=True,
    )

    # Enable and start the service
    result = subprocess.run(
        ["systemctl", "--user", "enable", "--now", LINUX_SERVICE_NAME],
        capture_output=True,
        text=True,
    )

    if result.returncode != 0:
        return False, f"Failed to enable service: {result.stderr}"

    return True, f"Service installed at {service_path}"


def _uninstall_linux_service() -> tuple[bool, str]:
    """Uninstall systemd user service on Linux."""
    service_path = _get_systemd_service_path()

    if not service_path.exists():
        return False, "Service is not installed"

    # Stop and disable the service
    subprocess.run(
        ["systemctl", "--user", "disable", "--now", LINUX_SERVICE_NAME],
        capture_output=True,
    )

    # Remove service file
    service_path.unlink()

    # Reload systemd
    subprocess.run(
        ["systemctl", "--user", "daemon-reload"],
        capture_output=True,
    )

    return True, "Service stopped and removed"


def _get_linux_service_status() -> dict:
    """Get systemd user service status on Linux."""
    service_path = _get_systemd_service_path()

    if not service_path.exists():
        return {"installed": False, "running": False, "path": str(service_path)}

    # Check if service is running
    result = subprocess.run(
        ["systemctl", "--user", "is-active", LINUX_SERVICE_NAME],
        capture_output=True,
        text=True,
    )

    running = result.stdout.strip() == "active"

    # Get PID if running
    pid = None
    if running:
        pid_result = subprocess.run(
            ["systemctl", "--user", "show", LINUX_SERVICE_NAME, "--property=MainPID"],
            capture_output=True,
            text=True,
        )
        if pid_result.returncode == 0:
            try:
                pid = int(pid_result.stdout.strip().split("=")[1])
            except (ValueError, IndexError):
                pass

    return {
        "installed": True,
        "running": running,
        "pid": pid,
        "path": str(service_path),
    }


# =============================================================================
# Public API
# =============================================================================


def install_service() -> tuple[bool, str]:
    """Install repowire daemon as a system service.

    Returns:
        Tuple of (success, message)
    """
    platform = get_platform()

    if platform == "macos":
        return _install_macos_service()
    elif platform == "linux":
        return _install_linux_service()
    else:
        return False, f"Unsupported platform: {sys.platform}"


def uninstall_service() -> tuple[bool, str]:
    """Uninstall repowire daemon system service.

    Returns:
        Tuple of (success, message)
    """
    platform = get_platform()

    if platform == "macos":
        return _uninstall_macos_service()
    elif platform == "linux":
        return _uninstall_linux_service()
    else:
        return False, f"Unsupported platform: {sys.platform}"


def get_service_status() -> dict:
    """Get the status of the repowire daemon service.

    Returns:
        Dict with keys: installed, running, pid (optional), path
    """
    platform = get_platform()

    if platform == "macos":
        return _get_macos_service_status()
    elif platform == "linux":
        return _get_linux_service_status()
    else:
        return {
            "installed": False,
            "running": False,
            "error": f"Unsupported platform: {sys.platform}",
        }
