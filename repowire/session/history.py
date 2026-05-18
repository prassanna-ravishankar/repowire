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

import base64
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
    line_offset: int = 0  # tie-breaker for same-timestamp turns within a session file


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
            for line_no, line in enumerate(f):
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
                    line_offset=line_no,
                )
    except OSError:
        return


def _sort_key(t: Turn) -> tuple[str, str, int]:
    return (t.timestamp, t.session_id, t.line_offset)


def load_peer_turns(peer_path: str | None, backend: str) -> list[Turn]:
    """Load and sort-merge all turns for a peer across session files.

    Newest-first. Returns empty list for non-claude-code backends (codex
    discovery is a follow-up). Sort key is the composite cursor tuple
    `(timestamp, session_id, line_offset)`; turns with empty timestamps
    sort to the end (oldest) in the newest-first ordering.
    """
    if backend != "claude-code":
        return []
    turns: list[Turn] = []
    for path in discover_claude_sessions(peer_path):
        turns.extend(_iter_claude_turns(path))
    turns.sort(key=_sort_key, reverse=True)
    return turns


def encode_cursor(t: Turn) -> str:
    """Pack a Turn's composite cursor into an opaque URL-safe token.

    Format (pre-encoding): `ts|session_id|line_offset`. The `|` separator
    cannot appear in ISO timestamps; session IDs are UUIDs or filenames
    without `|`. Base64-urlsafe encoding makes the token opaque on the wire.
    """
    raw = f"{t.timestamp}|{t.session_id}|{t.line_offset}".encode()
    return base64.urlsafe_b64encode(raw).decode().rstrip("=")


def decode_cursor(cursor: str) -> tuple[str, str, int] | None:
    """Decode an opaque cursor back into a `(timestamp, session_id, line_offset)` tuple.

    Returns None when the cursor is malformed — callers treat that as
    "ignore the cursor" (first page).
    """
    if not cursor:
        return None
    try:
        padding = "=" * (-len(cursor) % 4)
        raw = base64.urlsafe_b64decode(cursor + padding).decode()
    except (ValueError, UnicodeDecodeError):
        return None
    parts = raw.split("|", 2)
    if len(parts) != 3:
        return None
    ts, session_id, offset_str = parts
    try:
        offset = int(offset_str)
    except ValueError:
        return None
    return (ts, session_id, offset)


def page_turns(turns: list[Turn], limit: int, before: str | None) -> tuple[list[Turn], str | None]:
    """Slice a newest-first turn list by an opaque cursor.

    `before` is an opaque cursor produced by `encode_cursor` (or None for
    the first page). Returns `(page, next_cursor)`. `next_cursor` is the
    composite cursor of the last turn on the page when more results exist,
    else None.

    Empty-timestamp turns are included consistently in both first-page and
    paginated requests — their composite tuple `("", session, offset)` is
    compared the same way as any other turn.
    """
    if before:
        cursor = decode_cursor(before)
        if cursor is None:
            filtered = turns
        else:
            filtered = [t for t in turns if _sort_key(t) < cursor]
    else:
        filtered = turns
    page = filtered[:limit]
    next_cursor = encode_cursor(page[-1]) if len(filtered) > limit and page else None
    return page, next_cursor
