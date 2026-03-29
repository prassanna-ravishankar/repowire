"""Google Gemini CLI installer — hooks and MCP server configuration.

Gemini CLI stores both hooks and MCP servers in ~/.gemini/settings.json.
Hook events: SessionStart, BeforeAgent (≈UserPromptSubmit), AfterAgent (≈Stop).
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path

GEMINI_HOME = Path.home() / ".gemini"
SETTINGS_PATH = GEMINI_HOME / "settings.json"

# Gemini hook events we install
HOOK_EVENTS = ["SessionStart", "BeforeAgent", "AfterAgent"]


def _load_settings() -> dict:
    if not SETTINGS_PATH.exists():
        return {}
    try:
        return json.loads(SETTINGS_PATH.read_text())
    except json.JSONDecodeError as e:
        raise RuntimeError(
            f"Corrupted settings.json at {SETTINGS_PATH}: {e}. "
            "Please fix or delete the file manually."
        ) from e
    except OSError:
        return {}


def _save_settings(data: dict) -> None:
    GEMINI_HOME.mkdir(parents=True, exist_ok=True)
    tmp = SETTINGS_PATH.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, indent=2))
    tmp.chmod(0o600)
    tmp.replace(SETTINGS_PATH)


def _make_hook_entry(command: str, matcher: str | None = None) -> dict:
    entry: dict = {
        "hooks": [{"type": "command", "command": command}],
    }
    if matcher:
        entry["matcher"] = matcher
    return entry


def _is_repowire_hook(entry: dict) -> bool:
    """Check if a hook entry belongs to repowire."""
    for h in entry.get("hooks", []):
        if "repowire" in h.get("command", ""):
            return True
    return False


# Gemini uses BeforeAgent (≈UserPromptSubmit) and AfterAgent (≈Stop)
# Our hook handlers accept --backend=gemini to register with correct AgentType
_REPOWIRE_HOOKS = {
    "SessionStart": _make_hook_entry(
        "repowire hook session --backend=gemini", matcher="startup",
    ),
    "BeforeAgent": _make_hook_entry("repowire hook prompt --backend=gemini"),
    "AfterAgent": _make_hook_entry("repowire hook stop --backend=gemini"),
}


def install_hooks() -> bool:
    """Install repowire hooks into ~/.gemini/settings.json.

    Appends to existing hook arrays, preserving user-defined hooks.
    """
    data = _load_settings()
    hooks = data.setdefault("hooks", {})

    for event, entry in _REPOWIRE_HOOKS.items():
        existing = hooks.get(event, [])
        existing = [e for e in existing if not _is_repowire_hook(e)]
        existing.append(entry)
        hooks[event] = existing

    _save_settings(data)
    return True


def uninstall_hooks() -> bool:
    """Remove repowire hooks from settings.json, preserving user-defined hooks."""
    data = _load_settings()
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
        _save_settings(data)
    return removed


def install_mcp() -> bool:
    """Add repowire MCP server to ~/.gemini/settings.json."""
    data = _load_settings()
    servers = data.setdefault("mcpServers", {})

    if "repowire" in servers:
        return True  # already installed

    servers["repowire"] = {
        "command": "repowire",
        "args": ["mcp"],
    }

    _save_settings(data)
    return True


def uninstall_mcp() -> bool:
    """Remove repowire MCP server from settings.json."""
    data = _load_settings()
    servers = data.get("mcpServers", {})

    if "repowire" not in servers:
        return False

    del servers["repowire"]
    if not servers:
        data.pop("mcpServers", None)

    _save_settings(data)
    return True


def check_hooks_installed() -> bool:
    """Check if repowire hooks are configured in Gemini."""
    data = _load_settings()
    hooks = data.get("hooks", {})
    return any(event in hooks for event in HOOK_EVENTS)


def check_mcp_installed() -> bool:
    """Check if repowire MCP server is configured in Gemini."""
    data = _load_settings()
    return "repowire" in data.get("mcpServers", {})


def get_gemini_version() -> tuple[int, ...] | None:
    """Get Gemini CLI version as a tuple, or None if not installed."""
    try:
        result = subprocess.run(
            ["gemini", "--version"], capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        # Output varies — try to extract version number
        for part in result.stdout.strip().split():
            if part[0].isdigit():
                return tuple(int(x) for x in part.split("."))
        return None
    except (FileNotFoundError, subprocess.TimeoutExpired, ValueError, IndexError):
        return None
