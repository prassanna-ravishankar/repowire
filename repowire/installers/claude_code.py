from __future__ import annotations

import json
from pathlib import Path

CLAUDE_SETTINGS = Path.home() / ".claude" / "settings.json"

HOOK_EVENTS = ["Stop", "SessionStart", "SessionEnd", "UserPromptSubmit", "Notification"]


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


def install_hooks() -> bool:
    settings = _load_claude_settings()
    if "hooks" not in settings:
        settings["hooks"] = {}

    settings["hooks"]["Stop"] = [_make_hook_config("repowire hook stop")]
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
