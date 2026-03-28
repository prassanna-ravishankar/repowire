"""OpenAI Codex CLI installer — hooks and MCP server configuration."""

from __future__ import annotations

import json
import subprocess
from pathlib import Path

CODEX_HOME = Path.home() / ".codex"
HOOKS_PATH = CODEX_HOME / "hooks.json"
CONFIG_PATH = CODEX_HOME / "config.toml"

HOOK_EVENTS = ["SessionStart", "Stop", "UserPromptSubmit"]


def _load_hooks() -> dict:
    if not HOOKS_PATH.exists():
        return {}
    try:
        return json.loads(HOOKS_PATH.read_text())
    except (json.JSONDecodeError, OSError):
        return {}


def _save_hooks(data: dict) -> None:
    CODEX_HOME.mkdir(parents=True, exist_ok=True)
    HOOKS_PATH.write_text(json.dumps(data, indent=2))


def _make_hook_entry(command: str, matcher: str | None = None) -> dict:
    entry: dict = {
        "hooks": [{"type": "command", "command": command}],
    }
    if matcher:
        entry["matcher"] = matcher
    return entry


_REPOWIRE_HOOKS = {
    "SessionStart": _make_hook_entry("repowire hook session --backend=codex", matcher="startup"),
    "Stop": _make_hook_entry("repowire hook stop --backend=codex"),
    "UserPromptSubmit": _make_hook_entry("repowire hook prompt --backend=codex"),
}


def _is_repowire_hook(entry: dict) -> bool:
    """Check if a hook entry belongs to repowire."""
    for h in entry.get("hooks", []):
        if "repowire" in h.get("command", ""):
            return True
    return False


def install_hooks() -> bool:
    """Install repowire hooks into ~/.codex/hooks.json.

    Appends to existing hook arrays rather than overwriting, preserving
    user-defined hooks for the same events.
    """
    data = _load_hooks()
    hooks = data.setdefault("hooks", {})

    for event, entry in _REPOWIRE_HOOKS.items():
        existing = hooks.get(event, [])
        # Remove any previous repowire entries, then append fresh
        existing = [e for e in existing if not _is_repowire_hook(e)]
        existing.append(entry)
        hooks[event] = existing

    _save_hooks(data)
    return True


def uninstall_hooks() -> bool:
    """Remove repowire hooks from hooks.json, preserving user-defined hooks."""
    data = _load_hooks()
    hooks = data.get("hooks", {})
    if not hooks:
        return False

    removed = False
    for event in HOOK_EVENTS:
        entries = hooks.get(event, [])
        filtered = [e for e in entries if not _is_repowire_hook(e)]
        if len(filtered) < len(entries):
            removed = True
            if filtered:
                hooks[event] = filtered
            else:
                del hooks[event]

    if not hooks:
        data.pop("hooks", None)

    if removed:
        _save_hooks(data)
    return removed


def install_mcp() -> bool:
    """Add repowire MCP server to ~/.codex/config.toml.

    Appends the [mcp_servers.repowire] section. Preserves existing content.
    """
    CODEX_HOME.mkdir(parents=True, exist_ok=True)

    # Use bare command name — resolved via PATH at runtime, survives upgrades
    section = (
        "\n[mcp_servers.repowire]\n"
        'command = "repowire"\n'
        'args = ["mcp"]\n'
    )

    if CONFIG_PATH.exists():
        content = CONFIG_PATH.read_text()
        if "[mcp_servers.repowire]" in content:
            return True  # already installed
        CONFIG_PATH.write_text(content.rstrip() + "\n" + section)
    else:
        CONFIG_PATH.write_text(section.lstrip())

    return True


def uninstall_mcp() -> bool:
    """Remove repowire MCP server from config.toml."""
    if not CONFIG_PATH.exists():
        return False

    content = CONFIG_PATH.read_text()
    if "[mcp_servers.repowire]" not in content:
        return False

    # Remove the section and its key-value lines
    lines = content.splitlines(keepends=True)
    new_lines: list[str] = []
    in_section = False
    for line in lines:
        if line.strip() == "[mcp_servers.repowire]":
            in_section = True
            continue
        if in_section and (line.startswith("[") or not line.strip()):
            if line.startswith("["):
                in_section = False
                new_lines.append(line)
            continue
        if not in_section:
            new_lines.append(line)

    CONFIG_PATH.write_text("".join(new_lines).strip() + "\n" if new_lines else "")
    return True


def check_hooks_installed() -> bool:
    """Check if repowire hooks are configured in Codex."""
    data = _load_hooks()
    hooks = data.get("hooks", {})
    return "Stop" in hooks or "SessionStart" in hooks


def check_mcp_installed() -> bool:
    """Check if repowire MCP server is configured in Codex."""
    if not CONFIG_PATH.exists():
        return False
    return "[mcp_servers.repowire]" in CONFIG_PATH.read_text()


def get_codex_version() -> tuple[int, ...] | None:
    """Get Codex CLI version as a tuple, or None if not installed."""
    try:
        result = subprocess.run(
            ["codex", "--version"], capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        # Output format: "codex-cli 0.111.0"
        parts = result.stdout.strip().split()
        version_str = parts[1] if len(parts) >= 2 else parts[0]
        return tuple(int(x) for x in version_str.split("."))
    except (FileNotFoundError, subprocess.TimeoutExpired, ValueError, IndexError):
        return None
