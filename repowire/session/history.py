"""Full-replay transcript parser and per-peer session discovery.

Companion to `transcript.py` (last-turn only). Used by the
`GET /peers/{name}/transcript` daemon route.

v1 supports Claude Code on-disk transcripts:
`~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl`. The cwd is encoded
by replacing path separators with `-` (matching Claude Code's own scheme).

Codex transcripts (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`) are
date-bucketed rather than keyed by cwd, so per-peer discovery there
requires scanning rollout headers — deferred to a follow-up issue.
"""

from __future__ import annotations

import json
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from repowire.session.transcript import _summarize_tool_input


@dataclass
class Turn:
    """One user or assistant turn from a transcript replay."""

    role: str  # "user" | "assistant"
    text: str
    timestamp: str  # ISO-8601; "" if missing
    session_id: str
    tool_calls: list[dict[str, str]]  # [{name, input}] — empty for user turns


def _claude_projects_dir() -> Path:
    return Path.home() / ".claude" / "projects"


def _encode_cwd(path: str) -> str:
    """Claude Code encodes the cwd by replacing `/` with `-`.

    Example: `/Users/x/dev/repo` → `-Users-x-dev-repo`.
    """
    return path.replace("/", "-")


def discover_claude_sessions(peer_path: str | None) -> list[Path]:
    """List Claude JSONL transcripts for a peer's working directory.

    Returns empty list if the peer has no path or the project dir is absent.
    """
    if not peer_path:
        return []
    project_dir = _claude_projects_dir() / _encode_cwd(peer_path)
    if not project_dir.is_dir():
        return []
    return sorted(project_dir.glob("*.jsonl"))


def _extract_text(content: Any) -> str:
    """Concatenate text fragments from a Claude message `content` value."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, dict) and item.get("type") == "text":
                text = item.get("text", "")
                if text:
                    parts.append(text)
            elif isinstance(item, str):
                parts.append(item)
        return "\n".join(parts)
    if isinstance(content, dict) and content.get("type") == "text":
        return content.get("text", "") or ""
    return ""


def _iter_claude_turns(path: Path) -> Iterator[Turn]:
    """Yield Turn objects from a Claude Code JSONL transcript.

    Skips tool-only assistant entries (no text) and tool_result-only user
    entries, which match the semantics of the dashboard's live chat_turn ring.
    """
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue
                entry_type = entry.get("type")
                if entry_type not in ("user", "assistant"):
                    continue
                message = entry.get("message", {})
                if not isinstance(message, dict):
                    continue
                content = message.get("content", [])

                if entry_type == "user" and isinstance(content, list) and content and all(
                    isinstance(c, dict) and c.get("type") == "tool_result" for c in content
                ):
                    continue

                text = _extract_text(content)
                tool_calls: list[dict[str, str]] = []
                if entry_type == "assistant" and isinstance(content, list):
                    for item in content:
                        if isinstance(item, dict) and item.get("type") == "tool_use":
                            tool_calls.append({
                                "name": item.get("name", "unknown"),
                                "input": _summarize_tool_input(
                                    item.get("name", "unknown"),
                                    item.get("input", {}),
                                ),
                            })

                if not text and not tool_calls:
                    continue

                yield Turn(
                    role=entry_type,
                    text=text,
                    timestamp=entry.get("timestamp", "") or "",
                    session_id=entry.get("sessionId", "") or path.stem,
                    tool_calls=tool_calls,
                )
    except OSError:
        return


def load_peer_turns(peer_path: str | None, backend: str) -> list[Turn]:
    """Load and sort-merge all turns for a peer across session files.

    Newest-first. Returns empty list for non-claude-code backends (codex
    discovery is a follow-up). Sort key is (timestamp, session_id) so
    turns with missing or duplicate timestamps remain deterministic.
    """
    if backend != "claude-code":
        return []
    turns: list[Turn] = []
    for path in discover_claude_sessions(peer_path):
        turns.extend(_iter_claude_turns(path))
    turns.sort(key=lambda t: (t.timestamp, t.session_id), reverse=True)
    return turns


def page_turns(turns: list[Turn], limit: int, before: str | None) -> tuple[list[Turn], str | None]:
    """Slice a newest-first turn list by cursor.

    `before` is an ISO-8601 timestamp; only turns strictly older than it are
    returned. Returns (page, next_before). `next_before` is the oldest
    timestamp in the page when more results exist, else None.
    """
    if before:
        filtered = [t for t in turns if t.timestamp and t.timestamp < before]
    else:
        filtered = turns
    page = filtered[:limit]
    next_before = page[-1].timestamp if len(filtered) > limit and page else None
    return page, next_before
