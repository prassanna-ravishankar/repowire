"""Claude Code transcript parser."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def extract_last_turn_pair(transcript_path: Path) -> tuple[str | None, str | None]:
    """Single-pass extraction of last user prompt and last assistant response.

    Returns (user_text, assistant_text), either may be None.
    """
    if not transcript_path.exists():
        return None, None

    last_user: str | None = None
    last_assistant: str | None = None

    with open(transcript_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue

            entry_type = entry.get("type")
            message = entry.get("message", {})
            content = message.get("content", [])
            text = _extract_text_from_content(content)

            if entry_type == "user" and text:
                last_user = text
            elif entry_type == "assistant" and text:
                last_assistant = text

    return last_user, last_assistant


def _extract_text_from_content(content: Any) -> str | None:
    if isinstance(content, str):
        return content

    if isinstance(content, list):
        texts = []
        for item in content:
            if isinstance(item, dict):
                if item.get("type") == "text":
                    text = item.get("text", "")
                    if text:
                        texts.append(text)
            elif isinstance(item, str):
                texts.append(item)
        return " ".join(texts) if texts else None

    if isinstance(content, dict):
        if content.get("type") == "text":
            return content.get("text")
        if content.get("type") == "output":
            data = content.get("data", {})
            if isinstance(data, dict):
                inner_msg = data.get("message", {})
                if isinstance(inner_msg, dict):
                    return _extract_text_from_content(inner_msg.get("content"))

    return None
