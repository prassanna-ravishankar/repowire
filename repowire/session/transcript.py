"""Claude Code transcript parser."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def extract_last_assistant_response(transcript_path: Path) -> str | None:
    """Extract the last assistant response from a Claude Code transcript.

    Args:
        transcript_path: Path to the JSONL transcript file.

    Returns:
        The text content of the last assistant message, or None if not found.
    """
    if not transcript_path.exists():
        return None

    last_assistant_content: str | None = None

    with open(transcript_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue

            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue

            # Check entry type (top-level type field)
            if entry.get("type") != "assistant":
                continue

            message = entry.get("message", {})
            content = message.get("content", [])
            text = _extract_text_from_content(content)
            if text:
                last_assistant_content = text

    return last_assistant_content


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
