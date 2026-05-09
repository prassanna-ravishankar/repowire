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
    last_assistant_had_text: bool = False

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
            elif entry_type == "assistant":
                # Always track the flag: if this entry had no text (pure tool-use),
                # don't re-emit the previous text turn when stop fires for this entry.
                last_assistant_had_text = text is not None
                if text:
                    last_assistant = text

    return last_user, (last_assistant if last_assistant_had_text else None)


def extract_last_turn_tool_calls(transcript_path: Path) -> list[dict[str, str]]:
    """Extract tool calls from the last assistant turn.

    Reads the transcript backwards from the last assistant message,
    collecting tool_use entries until the previous user message.

    Returns list of {"name": "...", "input": "one-line summary"}.
    """
    if not transcript_path.exists():
        return []

    entries: list[dict[str, Any]] = []
    with open(transcript_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                entries.append(json.loads(line))
            except json.JSONDecodeError:
                continue

    # Walk backwards: collect tool_use from assistant entries until we hit a real user message
    tool_calls: list[dict[str, str]] = []
    found_assistant = False
    for entry in reversed(entries):
        entry_type = entry.get("type")
        if entry_type == "user" and found_assistant:
            # Skip tool_result entries (they have type=user but aren't real user messages)
            content = entry.get("message", {}).get("content", [])
            is_tool_result = isinstance(content, list) and all(
                isinstance(c, dict) and c.get("type") == "tool_result" for c in content
            )
            if not is_tool_result:
                break
            continue
        if entry_type != "assistant":
            continue
        found_assistant = True
        content = entry.get("message", {}).get("content", [])
        if not isinstance(content, list):
            continue
        for item in content:
            if isinstance(item, dict) and item.get("type") == "tool_use":
                name = item.get("name", "unknown")
                tool_input = item.get("input", {})
                summary = _summarize_tool_input(name, tool_input)
                tool_calls.append({"name": name, "input": summary})

    tool_calls.reverse()  # chronological order
    return tool_calls


def extract_last_turn_raw_tool_calls(transcript_path: Path) -> list[dict[str, Any]]:
    """Like extract_last_turn_tool_calls but returns raw tool inputs.

    Used by the ask-ack reminder logic to detect ack(corr_id=X) and
    ask(reply_to=X) invocations in the just-completed turn — those need to
    inspect the unsummarized arguments. Mirrors the same walk-backwards
    logic as extract_last_turn_tool_calls.

    Returns list of {"name": str, "input": dict | Any}.
    """
    if not transcript_path.exists():
        return []

    entries: list[dict[str, Any]] = []
    with open(transcript_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                entries.append(json.loads(line))
            except json.JSONDecodeError:
                continue

    raw_calls: list[dict[str, Any]] = []
    found_assistant = False
    for entry in reversed(entries):
        entry_type = entry.get("type")
        if entry_type == "user" and found_assistant:
            content = entry.get("message", {}).get("content", [])
            is_tool_result = isinstance(content, list) and all(
                isinstance(c, dict) and c.get("type") == "tool_result" for c in content
            )
            if not is_tool_result:
                break
            continue
        if entry_type != "assistant":
            continue
        found_assistant = True
        content = entry.get("message", {}).get("content", [])
        if not isinstance(content, list):
            continue
        for item in content:
            if isinstance(item, dict) and item.get("type") == "tool_use":
                raw_calls.append({
                    "name": item.get("name", "unknown"),
                    "input": item.get("input", {}),
                })

    raw_calls.reverse()
    return raw_calls


def _summarize_tool_input(name: str, tool_input: Any) -> str:
    """Create a one-line summary of tool input."""
    if not isinstance(tool_input, dict):
        return str(tool_input)[:80]

    # File operations: show the path
    if "file_path" in tool_input:
        return tool_input["file_path"].split("/")[-1]
    # Bash: show the command
    if "command" in tool_input:
        return tool_input["command"][:80]
    # Search: show the pattern
    if "pattern" in tool_input:
        return f"{tool_input['pattern']}"
    # Glob
    if "pattern" in tool_input:
        return tool_input["pattern"]
    # MCP tools
    if "peer_name" in tool_input:
        return f"→ {tool_input['peer_name']}"
    if "description" in tool_input:
        return tool_input["description"][:60]
    # Fallback: first string value
    for v in tool_input.values():
        if isinstance(v, str) and v:
            return v[:60]
    return ""


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
