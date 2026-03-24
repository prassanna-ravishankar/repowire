from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

CLAUDE_SETTINGS = Path.home() / ".claude" / "settings.json"
CLAUDE_JSON = Path.home() / ".claude.json"

HOOK_EVENTS = ["Stop", "SessionStart", "SessionEnd", "UserPromptSubmit", "Notification"]

# Channel transport requires Claude Code v2.1.80+ with claude.ai login
CHANNEL_MIN_VERSION = (2, 1, 80)


def _load_claude_settings() -> dict:
    if not CLAUDE_SETTINGS.exists():
        return {}
    try:
        with open(CLAUDE_SETTINGS) as f:
            return json.load(f)
    except json.JSONDecodeError as e:
        raise RuntimeError(
            f"Corrupted settings.json at {CLAUDE_SETTINGS}: {e}. "
            "Please fix or delete the file manually."
        ) from e


def _save_claude_settings(settings: dict) -> None:
    CLAUDE_SETTINGS.parent.mkdir(parents=True, exist_ok=True)
    with open(CLAUDE_SETTINGS, "w") as f:
        json.dump(settings, f, indent=2)


def _make_hook_config(command: str) -> dict:
    return {
        "hooks": [
            {
                "type": "command",
                "command": command,
            }
        ]
    }


def _make_notification_hook_config(command: str, matcher: str) -> dict:
    return {
        "matcher": matcher,
        "hooks": [
            {
                "type": "command",
                "command": command,
            }
        ],
    }


def install_hooks(channel_mode: bool = False) -> bool:
    """Install hooks. In channel_mode, only install Stop hook for dashboard chat_turns."""
    settings = _load_claude_settings()
    if "hooks" not in settings:
        settings["hooks"] = {}

    # Stop hook always needed (dashboard chat_turns)
    settings["hooks"]["Stop"] = [_make_hook_config("repowire hook stop")]

    if not channel_mode:
        # Full hook set for tmux transport
        settings["hooks"]["SessionStart"] = [_make_hook_config("repowire hook session")]
        settings["hooks"]["SessionEnd"] = [_make_hook_config("repowire hook session")]
        settings["hooks"]["UserPromptSubmit"] = [_make_hook_config("repowire hook prompt")]
        settings["hooks"]["Notification"] = [
            _make_notification_hook_config("repowire hook notification", "idle_prompt")
        ]

    _save_claude_settings(settings)
    return True


def uninstall_hooks() -> bool:
    """Remove repowire hooks. Returns True if hooks were removed, False if none existed."""
    settings = _load_claude_settings()

    if "hooks" not in settings:
        return False

    removed_any = False
    for event in HOOK_EVENTS:
        if event in settings["hooks"]:
            del settings["hooks"][event]
            removed_any = True

    if not settings["hooks"]:
        del settings["hooks"]

    if removed_any:
        _save_claude_settings(settings)
    return removed_any


def check_hooks_installed() -> bool:
    settings = _load_claude_settings()
    if "hooks" not in settings:
        return False

    return all(event in settings["hooks"] for event in HOOK_EVENTS)


# -- Channel transport --


def get_claude_version() -> tuple[int, ...] | None:
    """Get Claude Code version as a tuple, or None if not installed."""
    try:
        result = subprocess.run(
            ["claude", "--version"], capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        # Output like "2.1.81 (Claude Code)"
        version_str = result.stdout.strip().split()[0]
        return tuple(int(x) for x in version_str.split("."))
    except (FileNotFoundError, subprocess.TimeoutExpired, ValueError, IndexError):
        return None


def supports_channels() -> bool:
    """Check if Claude Code supports the channel transport."""
    version = get_claude_version()
    if not version:
        return False
    return version >= CHANNEL_MIN_VERSION


def _find_channel_server() -> Path | None:
    """Find the channel server.ts in the installed package."""
    # Check installed package location
    import repowire

    pkg_dir = Path(repowire.__file__).parent
    server = pkg_dir / "channel" / "server.ts"
    if server.exists():
        return server
    return None


def _has_bun() -> bool:
    """Check if bun runtime is available."""
    return shutil.which("bun") is not None


def install_channel() -> tuple[bool, str]:
    """Install the channel transport. Returns (success, message).

    Gracefully falls back with a clear message if:
    - Claude Code version too old
    - bun not installed
    - Channel server not found
    - claude.ai login required (detected at runtime)
    """
    if not _has_bun():
        return False, "bun runtime not found. Install from https://bun.sh"

    if not supports_channels():
        version = get_claude_version()
        v_str = ".".join(str(x) for x in version) if version else "unknown"
        return False, (
            f"Claude Code {v_str} doesn't support channels "
            f"(requires {'.'.join(str(x) for x in CHANNEL_MIN_VERSION)}+). "
            "Using hooks instead."
        )

    server_path = _find_channel_server()
    if not server_path:
        return False, "Channel server.ts not found in package."

    # Install deps (bun install is idempotent — fast no-op if already installed)
    try:
        result = subprocess.run(
            ["bun", "install"], cwd=str(server_path.parent),
            capture_output=True, timeout=30,
        )
        if result.returncode != 0:
            return False, f"bun install failed: {result.stderr.decode()[:200]}"
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return False, "Failed to install channel dependencies."

    # Add to ~/.claude.json (user-level MCP config)
    try:
        config = json.loads(CLAUDE_JSON.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        config = {}
    config.setdefault("mcpServers", {})

    config["mcpServers"]["repowire-channel"] = {
        "command": "bun",
        "args": [str(server_path)],
    }

    CLAUDE_JSON.write_text(json.dumps(config, indent=2))

    return True, (
        "Channel transport installed. "
        "Start Claude with: claude --dangerously-load-development-channels server:repowire-channel"
    )


def uninstall_channel() -> bool:
    """Remove the channel from ~/.claude.json."""
    if not CLAUDE_JSON.exists():
        return False

    try:
        config = json.loads(CLAUDE_JSON.read_text())
    except json.JSONDecodeError:
        return False

    servers = config.get("mcpServers", {})
    if "repowire-channel" not in servers:
        return False

    del servers["repowire-channel"]
    if not servers:
        del config["mcpServers"]

    CLAUDE_JSON.write_text(json.dumps(config, indent=2))
    return True


def check_channel_installed() -> bool:
    """Check if the channel transport is configured."""
    if not CLAUDE_JSON.exists():
        return False
    try:
        config = json.loads(CLAUDE_JSON.read_text())
        return "repowire-channel" in config.get("mcpServers", {})
    except json.JSONDecodeError:
        return False
