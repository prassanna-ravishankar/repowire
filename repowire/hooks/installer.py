from __future__ import annotations

import json
import shutil
from pathlib import Path

HOOKS_DIR = Path.home() / ".repowire" / "hooks"
CLAUDE_SETTINGS = Path.home() / ".claude" / "settings.json"
STOP_HANDLER_NAME = "stop_handler.py"


def _get_stop_handler_source() -> Path:
    return Path(__file__).parent / STOP_HANDLER_NAME


def _get_stop_handler_dest() -> Path:
    return HOOKS_DIR / STOP_HANDLER_NAME


def _load_claude_settings() -> dict:
    if not CLAUDE_SETTINGS.exists():
        return {}
    try:
        with open(CLAUDE_SETTINGS, "r") as f:
            return json.load(f)
    except json.JSONDecodeError:
        return {}


def _save_claude_settings(settings: dict) -> None:
    CLAUDE_SETTINGS.parent.mkdir(parents=True, exist_ok=True)
    with open(CLAUDE_SETTINGS, "w") as f:
        json.dump(settings, f, indent=2)


def install_hooks() -> bool:
    HOOKS_DIR.mkdir(parents=True, exist_ok=True)

    source = _get_stop_handler_source()
    dest = _get_stop_handler_dest()
    shutil.copy2(source, dest)
    dest.chmod(0o755)

    pending_dir = Path.home() / ".repowire" / "pending"
    pending_dir.mkdir(parents=True, exist_ok=True)

    settings = _load_claude_settings()
    if "hooks" not in settings:
        settings["hooks"] = {}

    hook_config = {
        "hooks": [
            {
                "type": "command",
                "command": f"python3 {dest}",
            }
        ]
    }
    settings["hooks"]["Stop"] = [hook_config]

    _save_claude_settings(settings)
    return True


def uninstall_hooks() -> bool:
    settings = _load_claude_settings()

    if "hooks" not in settings:
        return True

    dest = _get_stop_handler_dest()
    hook_command = f"python3 {dest}"

    if "Stop" in settings["hooks"]:
        hooks = settings["hooks"]["Stop"]
        if isinstance(hooks, list):
            settings["hooks"]["Stop"] = [
                h
                for h in hooks
                if not (
                    isinstance(h, dict)
                    and any(
                        hh.get("command") == hook_command
                        for hh in h.get("hooks", [])
                        if isinstance(hh, dict)
                    )
                )
            ]
            if not settings["hooks"]["Stop"]:
                del settings["hooks"]["Stop"]

    if settings.get("hooks") and not settings["hooks"]:
        del settings["hooks"]

    _save_claude_settings(settings)

    if dest.exists():
        dest.unlink()

    return True


def check_hooks_installed() -> bool:
    dest = _get_stop_handler_dest()
    if not dest.exists():
        return False

    settings = _load_claude_settings()
    if "hooks" not in settings or "Stop" not in settings["hooks"]:
        return False

    hook_command = f"python3 {dest}"
    hooks = settings["hooks"]["Stop"]

    if not isinstance(hooks, list):
        return False

    for hook_config in hooks:
        if not isinstance(hook_config, dict):
            continue
        for h in hook_config.get("hooks", []):
            if isinstance(h, dict) and h.get("command") == hook_command:
                return True

    return False
