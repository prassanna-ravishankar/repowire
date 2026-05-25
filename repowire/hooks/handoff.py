"""Bounded session handoff summaries for hook-level resume context."""

from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

HANDOFF_WORD_LIMIT = 300
HANDOFF_TURN_LIMIT = 12


def _cache_dir() -> Path:
    from repowire.config import models

    return models.CACHE_DIR / "handoffs"


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _identity_key(cwd: str | None, backend: str, session_id: str | None) -> str | None:
    if not cwd or not session_id:
        return None
    try:
        normalized_cwd = str(Path(cwd).expanduser().resolve())
    except OSError:
        normalized_cwd = str(Path(cwd).expanduser())
    raw = json.dumps(
        {"backend": backend, "cwd": normalized_cwd, "session_id": session_id},
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(raw.encode()).hexdigest()


def _handoff_path(cwd: str | None, backend: str, session_id: str | None) -> Path | None:
    key = _identity_key(cwd, backend, session_id)
    if key is None:
        return None
    return _cache_dir() / f"{key}.json"


def _text_from_content(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, dict):
                block_type = block.get("type")
                if block_type == "text" and isinstance(block.get("text"), str):
                    parts.append(block["text"])
                elif block_type == "tool_use":
                    name = block.get("name") or "tool"
                    parts.append(f"[tool: {name}]")
                elif block_type == "tool_result":
                    parts.append("[tool result]")
            elif isinstance(block, str):
                parts.append(block)
        return " ".join(parts)
    return ""


def _entry_turn(entry: dict[str, Any]) -> tuple[str, str] | None:
    role = entry.get("type") or entry.get("role")
    message = entry.get("message")
    content: Any = None
    if isinstance(message, dict):
        role = message.get("role") or role
        content = message.get("content")
    elif isinstance(entry.get("content"), (str, list)):
        content = entry.get("content")
    if role not in {"user", "assistant"}:
        return None
    text = _clean_text(_text_from_content(content))
    if not text:
        return None
    return str(role), text


def _clean_text(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def _cap_words(text: str, limit: int = HANDOFF_WORD_LIMIT) -> str:
    words = text.split()
    if len(words) <= limit:
        return text
    return " ".join(words[:limit]).rstrip(".,;:") + "..."


def build_handoff_summary(
    *,
    transcript_path: Path | None = None,
    user_text: str | None = None,
    assistant_text: str | None = None,
) -> str | None:
    """Build a compact resume summary from source-owned turn data."""
    turns: list[tuple[str, str]] = []
    if transcript_path and transcript_path.exists():
        try:
            with open(transcript_path) as f:
                for line in f:
                    try:
                        entry = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if isinstance(entry, dict) and (turn := _entry_turn(entry)):
                        turns.append(turn)
        except OSError:
            turns = []

    if not turns:
        if user_text:
            turns.append(("user", _clean_text(user_text)))
        if assistant_text:
            turns.append(("assistant", _clean_text(assistant_text)))

    turns = [(role, text) for role, text in turns if text][-HANDOFF_TURN_LIMIT:]
    if not turns:
        return None

    parts = [f"{role}: {text}" for role, text in turns]
    return _cap_words("Recent session context: " + " ".join(parts))


def write_handoff_summary(
    *,
    cwd: str | None,
    backend: str,
    session_id: str | None,
    transcript_path: Path | None = None,
    user_text: str | None = None,
    assistant_text: str | None = None,
) -> None:
    """Persist only a bounded summary for exact same-session resume injection."""
    path = _handoff_path(cwd, backend, session_id)
    if path is None:
        return
    summary = build_handoff_summary(
        transcript_path=transcript_path,
        user_text=user_text,
        assistant_text=assistant_text,
    )
    if not summary:
        return
    payload = {
        "backend": backend,
        "cwd": str(Path(cwd or "").expanduser()),
        "session_id": session_id,
        "summary": summary,
        "word_limit": HANDOFF_WORD_LIMIT,
        "updated_at": _now(),
    }
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(payload, sort_keys=True, indent=2) + "\n")
    except OSError:
        return


def load_handoff_context(
    *,
    cwd: str | None,
    backend: str,
    session_id: str | None,
) -> str | None:
    """Return a clearly labeled handoff block for the same session identity."""
    path = _handoff_path(cwd, backend, session_id)
    if path is None or not path.exists():
        return None
    try:
        payload = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    summary = payload.get("summary")
    if not isinstance(summary, str) or not summary.strip():
        return None
    return "\n".join([
        "[Repowire Session Handoff]",
        "Summary from the previous turn of this same cwd/session identity:",
        _cap_words(_clean_text(summary)),
    ])
