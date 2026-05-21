"""Diagnostic checks for `repowire doctor`.

Each check is a pure function returning a CheckResult. The CLI layer
(see cli.py `doctor` command) handles printing and exit codes.

Status semantics:
- OK    — green ✓ — all good
- WARN  — yellow ⚠ — degraded/non-fatal (missing optional feature, soft config issue)
- FAIL  — red ✗ — broken; doctor exits 1 if any FAIL present
- SKIP  — dim ·  — check not applicable in this environment
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path

import httpx

from repowire import __version__
from repowire.config.models import Config


class Status(str, Enum):
    OK = "ok"
    WARN = "warn"
    FAIL = "fail"
    SKIP = "skip"


@dataclass
class CheckResult:
    name: str
    status: Status
    detail: str = ""
    children: list[CheckResult] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Individual checks
# ---------------------------------------------------------------------------


def check_python_version(min_version: tuple[int, int] = (3, 10)) -> CheckResult:
    cur = sys.version_info[:2]
    if cur >= min_version:
        return CheckResult(
            "Python version",
            Status.OK,
            f"{cur[0]}.{cur[1]} (>= {min_version[0]}.{min_version[1]})",
        )
    return CheckResult(
        "Python version",
        Status.FAIL,
        f"{cur[0]}.{cur[1]} — requires >= {min_version[0]}.{min_version[1]}",
    )


def check_tmux() -> CheckResult:
    path = shutil.which("tmux")
    if not path:
        return CheckResult("tmux available", Status.WARN, "not found on PATH")
    try:
        result = subprocess.run(
            ["tmux", "-V"], capture_output=True, text=True, timeout=5,
        )
        version = result.stdout.strip() or "unknown version"
    except (subprocess.TimeoutExpired, FileNotFoundError):
        version = "version check failed"
    return CheckResult("tmux available", Status.OK, version)


def check_package_manager() -> CheckResult:
    for tool in ("uv", "pipx", "pip"):
        if shutil.which(tool):
            return CheckResult("Package manager (for updates)", Status.OK, tool)
    return CheckResult(
        "Package manager (for updates)",
        Status.WARN,
        "none of uv/pipx/pip found on PATH",
    )


def check_daemon(daemon_url: str, timeout: float = 2.0) -> CheckResult:
    """GET /health on the configured daemon URL."""
    try:
        with httpx.Client(timeout=timeout) as client:
            resp = client.get(f"{daemon_url}/health")
            resp.raise_for_status()
            payload = resp.json()
    except httpx.ConnectError:
        return CheckResult("Daemon reachable", Status.FAIL, f"connect failed: {daemon_url}")
    except httpx.HTTPError as exc:
        return CheckResult("Daemon reachable", Status.FAIL, f"{daemon_url}: {exc}")
    except Exception as exc:  # noqa: BLE001
        return CheckResult("Daemon reachable", Status.FAIL, f"{daemon_url}: {exc}")

    version = payload.get("version", "?")
    relay_mode = " (relay mode)" if payload.get("relay_mode") else ""
    return CheckResult(
        "Daemon reachable",
        Status.OK,
        f"{daemon_url} — v{version}{relay_mode}",
    )


def check_runtimes() -> CheckResult:
    """Per-runtime hook + MCP install state. Aggregated into one parent result."""
    children: list[CheckResult] = []

    children.append(_check_claude_code())
    children.append(_check_codex())
    children.append(_check_gemini())
    children.append(_check_opencode())
    children.append(_check_pi())

    detected = [c for c in children if c.status is not Status.SKIP]
    if not detected:
        return CheckResult(
            "Agent runtimes",
            Status.WARN,
            "no runtimes detected",
            children=children,
        )

    worst = _worst(detected)
    detail = f"{len(detected)} detected"
    return CheckResult("Agent runtimes", worst, detail, children=children)


def _check_claude_code() -> CheckResult:
    if not shutil.which("claude"):
        return CheckResult("claude-code", Status.SKIP, "CLI not on PATH")
    from repowire.installers.claude_code import (
        check_channel_installed,
        check_hooks_installed,
        get_claude_version,
    )

    version = get_claude_version()
    ver_str = ".".join(str(x) for x in version) if version else "unknown"

    if not check_hooks_installed():
        return CheckResult(
            "claude-code",
            Status.FAIL,
            f"v{ver_str} — hooks not installed (run `repowire setup`)",
        )
    channel = " + channel" if check_channel_installed() else ""
    return CheckResult("claude-code", Status.OK, f"v{ver_str} — hooks{channel} installed")


def _check_codex() -> CheckResult:
    if not shutil.which("codex") and not (Path.home() / ".codex").exists():
        return CheckResult("codex", Status.SKIP, "not detected")
    from repowire.installers.codex import (
        check_hooks_installed,
        check_mcp_installed,
        get_codex_version,
    )

    version = get_codex_version()
    ver_str = ".".join(str(x) for x in version) if version else "unknown"

    hooks_ok = check_hooks_installed()
    mcp_ok = check_mcp_installed()
    if hooks_ok and mcp_ok:
        return CheckResult("codex", Status.OK, f"v{ver_str} — hooks + MCP installed")
    missing = []
    if not hooks_ok:
        missing.append("hooks")
    if not mcp_ok:
        missing.append("MCP")
    return CheckResult(
        "codex",
        Status.FAIL,
        f"v{ver_str} — missing: {', '.join(missing)} (run `repowire setup`)",
    )


def _check_gemini() -> CheckResult:
    if not shutil.which("gemini") and not (Path.home() / ".gemini").exists():
        return CheckResult("gemini", Status.SKIP, "not detected")
    from repowire.installers.gemini import (
        check_hooks_installed,
        check_mcp_installed,
        get_gemini_version,
    )

    version = get_gemini_version()
    ver_str = ".".join(str(x) for x in version) if version else "unknown"

    hooks_ok = check_hooks_installed()
    mcp_ok = check_mcp_installed()
    if hooks_ok and mcp_ok:
        return CheckResult("gemini", Status.OK, f"v{ver_str} — hooks + MCP installed")
    missing = []
    if not hooks_ok:
        missing.append("hooks")
    if not mcp_ok:
        missing.append("MCP")
    return CheckResult(
        "gemini",
        Status.FAIL,
        f"v{ver_str} — missing: {', '.join(missing)} (run `repowire setup`)",
    )


def _check_opencode() -> CheckResult:
    if not shutil.which("opencode") and not (Path.home() / ".config" / "opencode").exists():
        return CheckResult("opencode", Status.SKIP, "not detected")
    from repowire.installers.opencode import check_plugin_installed

    if check_plugin_installed():
        return CheckResult("opencode", Status.OK, "plugin installed")
    return CheckResult(
        "opencode",
        Status.FAIL,
        "plugin not installed (run `repowire opencode install`)",
    )


def _check_pi() -> CheckResult:
    if not shutil.which("pi") and not (Path.home() / ".pi").exists():
        return CheckResult("pi", Status.SKIP, "not detected")
    from repowire.installers.pi import check_extension_installed

    if check_extension_installed():
        return CheckResult("pi", Status.OK, "extension installed")
    return CheckResult(
        "pi",
        Status.FAIL,
        "extension not installed (run `repowire setup`)",
    )


def check_spawn_allowlist(config: Config) -> CheckResult:
    spawn = config.daemon.spawn
    commands = spawn.commands
    if not commands and not spawn.allowed_paths:
        return CheckResult(
            "Spawn config",
            Status.SKIP,
            "not configured (spawn disabled)",
        )

    children: list[CheckResult] = []
    for backend, cmd in commands.items():
        # Allowed commands may have flags ("claude --dangerously-skip-permissions");
        # resolve the leading binary only.
        bin_name = cmd.split()[0] if cmd else cmd
        if shutil.which(bin_name):
            children.append(CheckResult(f"command: {backend.value} -> {cmd}", Status.OK))
        else:
            children.append(
                CheckResult(
                    f"command: {backend.value} -> {cmd}",
                    Status.WARN,
                    f"{bin_name} not on PATH",
                ),
            )
    for path_str in spawn.allowed_paths:
        path = Path(path_str).expanduser()
        if path.is_dir():
            children.append(CheckResult(f"path: {path_str}", Status.OK))
        else:
            children.append(
                CheckResult(f"path: {path_str}", Status.WARN, "not an existing directory"),
            )

    if not children:
        return CheckResult("Spawn config", Status.SKIP, "empty")
    worst = _worst(children)
    summary = (
        f"{len(commands)} command(s), {len(spawn.allowed_paths)} path(s)"
    )
    return CheckResult("Spawn config", worst, summary, children=children)


def check_auth_token(config: Config) -> CheckResult:
    if config.daemon.auth_token:
        return CheckResult(
            "WebSocket auth token", Status.OK, "set (required for connections)",
        )
    return CheckResult(
        "WebSocket auth token", Status.SKIP, "not set (open access on bind address)",
    )


def check_relay(config: Config, timeout: float = 5.0) -> CheckResult:
    if not config.relay.enabled:
        return CheckResult("Relay", Status.SKIP, "not enabled")

    url = config.relay.url
    api_key = config.relay.api_key
    if not url:
        return CheckResult("Relay", Status.FAIL, "enabled but no URL configured")

    relay_http = url.replace("wss://", "https://").replace("ws://", "http://")
    health_url = f"{relay_http.rstrip('/')}/health"
    try:
        with httpx.Client(timeout=timeout) as client:
            resp = client.get(health_url)
            resp.raise_for_status()
    except httpx.ConnectError:
        return CheckResult("Relay", Status.FAIL, f"unreachable: {health_url}")
    except httpx.HTTPStatusError as exc:
        return CheckResult("Relay", Status.WARN, f"{health_url}: HTTP {exc.response.status_code}")
    except Exception as exc:  # noqa: BLE001
        return CheckResult("Relay", Status.WARN, f"{health_url}: {exc}")

    key_state = "with API key" if api_key else "no API key (anonymous)"
    return CheckResult("Relay", Status.OK, f"reachable at {url} — {key_state}")


def check_channel_transport() -> CheckResult:
    """Channel transport is opt-in; only inspect if claude-code is detected."""
    if not shutil.which("claude"):
        return CheckResult(
            "Channel transport (experimental)", Status.SKIP, "claude-code not detected",
        )
    from repowire.installers.claude_code import check_channel_installed

    if not check_channel_installed():
        return CheckResult(
            "Channel transport (experimental)",
            Status.SKIP,
            "not configured (default hooks transport)",
        )

    bun = shutil.which("bun")
    if not bun:
        return CheckResult(
            "Channel transport (experimental)",
            Status.FAIL,
            "configured but `bun` not on PATH",
        )
    return CheckResult(
        "Channel transport (experimental)",
        Status.OK,
        f"configured, bun at {bun}",
    )


def check_repowire_version() -> CheckResult:
    return CheckResult("repowire version", Status.OK, __version__)


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------


_STATUS_RANK = {Status.OK: 0, Status.SKIP: 0, Status.WARN: 1, Status.FAIL: 2}


def _worst(results: list[CheckResult]) -> Status:
    if not results:
        return Status.OK
    return max((r.status for r in results), key=lambda s: _STATUS_RANK[s])


def run_all(config: Config, daemon_url: str) -> list[CheckResult]:
    """Run every check in display order and return the list of results."""
    return [
        check_repowire_version(),
        check_python_version(),
        check_tmux(),
        check_package_manager(),
        check_daemon(daemon_url),
        check_runtimes(),
        check_spawn_allowlist(config),
        check_auth_token(config),
        check_relay(config),
        check_channel_transport(),
    ]


def exit_code(results: list[CheckResult]) -> int:
    """1 if any result (including nested) is FAIL, else 0."""
    def has_fail(r: CheckResult) -> bool:
        if r.status is Status.FAIL:
            return True
        return any(has_fail(c) for c in r.children)

    return 1 if any(has_fail(r) for r in results) else 0
