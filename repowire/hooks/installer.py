from __future__ import annotations

import json
from pathlib import Path

CLAUDE_SETTINGS = Path.home() / ".claude" / "settings.json"


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


def _make_hook_config(command: str) -> dict:
    return {
        "hooks": [
            {
                "type": "command",
                "command": command,
            }
        ]
    }


def install_hooks(dev: bool = False) -> bool:
    pending_dir = Path.home() / ".repowire" / "pending"
    pending_dir.mkdir(parents=True, exist_ok=True)

    if dev:
        project_dir = Path(__file__).parent.parent.parent
        # Use uv run for dev mode to always run from source (no caching)
        base_cmd = f"uv run --directory {project_dir} repowire"
    else:
        base_cmd = "uvx repowire"

    settings = _load_claude_settings()
    if "hooks" not in settings:
        settings["hooks"] = {}

    settings["hooks"]["Stop"] = [_make_hook_config(f"{base_cmd} hook stop")]
    settings["hooks"]["SessionStart"] = [_make_hook_config(f"{base_cmd} hook session")]
    settings["hooks"]["SessionEnd"] = [_make_hook_config(f"{base_cmd} hook session")]

    _save_claude_settings(settings)
    return True


def uninstall_hooks() -> bool:
    settings = _load_claude_settings()

    if "hooks" not in settings:
        return True

    for event in ["Stop", "SessionStart", "SessionEnd"]:
        if event in settings["hooks"]:
            del settings["hooks"][event]

    if not settings["hooks"]:
        del settings["hooks"]

    _save_claude_settings(settings)
    return True


def check_hooks_installed() -> bool:
    settings = _load_claude_settings()
    if "hooks" not in settings:
        return False

    return all(
        event in settings["hooks"]
        for event in ["Stop", "SessionStart", "SessionEnd"]
    )
