"""Full-replay transcript parser and per-peer session discovery.

Companion to `transcript.py` (last-turn only). Used by the
`GET /peers/{name}/transcript` daemon route.

Supports backend-specific local history where the runtime exposes enough
metadata to map a transcript back to a peer path:

* Claude Code: `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl`
* Codex / codex-acp: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`, matched
  by `session_meta.payload.cwd` or `turn_context.payload.cwd`
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
    turn_id: str
    tool_calls: list[dict[str, str]]  # [{name, input}] — empty for user turns
    line_offset: int = 0  # tie-breaker for same-timestamp turns within a session file


@dataclass
class HistoryLoadResult:
    """Normalized local-history load result for a peer backend."""

    turns: list[Turn]
    status: str  # "available" | "unavailable" | "unsupported"
    backend: str
    message: str


def _claude_projects_dir() -> Path:
    return Path.home() / ".claude" / "projects"


def _codex_sessions_dir() -> Path:
    return Path.home() / ".codex" / "sessions"


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


def _path_matches(entry_path: str | None, peer_path: str) -> bool:
    if not entry_path:
        return False
    try:
        return Path(entry_path).expanduser().resolve() == Path(peer_path).expanduser().resolve()
    except OSError:
        return entry_path == peer_path


def _codex_file_matches_peer(path: Path, peer_path: str) -> bool:
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
                if entry.get("type") not in ("session_meta", "turn_context"):
                    continue
                payload = entry.get("payload", {})
                if isinstance(payload, dict) and _path_matches(payload.get("cwd"), peer_path):
                    return True
    except OSError:
        return False
    return False


def discover_codex_sessions(peer_path: str | None) -> list[Path]:
    """List Codex rollout JSONLs whose runtime cwd matches the peer path.

    Codex stores transcripts in date buckets, so discovery scans lightweight
    metadata entries instead of relying on path-derived directories.
    """
    if not peer_path:
        return []
    root = _codex_sessions_dir()
    if not root.is_dir():
        return []
    return sorted(
        path
        for path in root.glob("**/rollout-*.jsonl")
        if _codex_file_matches_peer(path, peer_path)
    )


def _extract_text(content: Any) -> str:
    """Concatenate text fragments from a Claude message `content` value."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, dict) and item.get("type") in (
                "text",
                "input_text",
                "output_text",
            ):
                text = item.get("text", "")
                if text:
                    parts.append(text)
            elif isinstance(item, str):
                parts.append(item)
        return "\n".join(parts)
    if isinstance(content, dict) and content.get("type") == "text":
        return content.get("text", "") or ""
    return ""


def _parse_codex_arguments(args_raw: Any) -> dict[str, Any]:
    if isinstance(args_raw, str):
        try:
            args = json.loads(args_raw) if args_raw else {}
        except json.JSONDecodeError:
            return {}
        return args if isinstance(args, dict) else {}
    if isinstance(args_raw, dict):
        return args_raw
    return {}


def _iter_claude_turns(path: Path) -> Iterator[Turn]:
    """Yield Turn objects from a Claude Code JSONL transcript.

    Skips tool-only assistant entries (no text) and tool_result-only user
    entries, which match the semantics of the dashboard's live chat_turn ring.
    """
    assistant_session_id = ""
    assistant_turn_id: str | None = None
    assistant_text: str | None = None
    assistant_timestamp = ""
    assistant_line_offset = 0
    assistant_tool_calls: list[dict[str, str]] = []

    def flush_assistant() -> Turn | None:
        nonlocal assistant_turn_id
        nonlocal assistant_text
        nonlocal assistant_timestamp
        nonlocal assistant_line_offset
        nonlocal assistant_tool_calls
        if not assistant_turn_id or not assistant_text:
            assistant_turn_id = None
            assistant_text = None
            assistant_timestamp = ""
            assistant_line_offset = 0
            assistant_tool_calls = []
            return None

        turn = Turn(
            role="assistant",
            text=assistant_text,
            timestamp=assistant_timestamp,
            session_id=assistant_session_id,
            turn_id=assistant_turn_id,
            tool_calls=assistant_tool_calls,
            line_offset=assistant_line_offset,
        )
        assistant_turn_id = None
        assistant_text = None
        assistant_timestamp = ""
        assistant_line_offset = 0
        assistant_tool_calls = []
        return turn

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
                session_id = entry.get("sessionId", "") or path.stem

                if entry_type == "user":
                    assistant_turn = flush_assistant()
                    if assistant_turn:
                        yield assistant_turn
                    if not text:
                        continue
                    yield Turn(
                        role="user",
                        text=text,
                        timestamp=entry.get("timestamp", "") or "",
                        session_id=session_id,
                        turn_id=f"history:{session_id}:{line_no}",
                        tool_calls=[],
                        line_offset=line_no,
                    )
                    continue

                if assistant_turn_id is None:
                    message_id = message.get("id") if isinstance(message, dict) else None
                    entry_id = entry.get("uuid") or entry.get("id")
                    assistant_turn_id = str(
                        entry_id or message_id or f"history:{session_id}:{line_no}"
                    )
                    assistant_session_id = session_id

                if isinstance(content, list):
                    for item in content:
                        if isinstance(item, dict) and item.get("type") == "tool_use":
                            assistant_tool_calls.append({
                                "name": item.get("name", "unknown"),
                                "input": _summarize_tool_input(
                                    item.get("name", "unknown"),
                                    item.get("input", {}),
                                ),
                            })

                if text:
                    assistant_text = text
                    assistant_timestamp = entry.get("timestamp", "") or ""
                    assistant_line_offset = line_no
            assistant_turn = flush_assistant()
            if assistant_turn:
                yield assistant_turn
    except OSError:
        return


def _iter_codex_turns(path: Path) -> Iterator[Turn]:
    """Yield Turn objects from a Codex rollout JSONL."""
    session_id = path.stem.removeprefix("rollout-")
    current_turn_id: str | None = None
    pending_user: tuple[str, str, int] | None = None
    assistant_tool_calls: list[dict[str, str]] = []

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
                payload = entry.get("payload", {})
                if entry_type == "session_meta" and isinstance(payload, dict):
                    raw_id = payload.get("id")
                    if isinstance(raw_id, str) and raw_id:
                        session_id = raw_id
                    continue

                if entry_type == "turn_context" and isinstance(payload, dict):
                    raw_turn_id = payload.get("turn_id")
                    if isinstance(raw_turn_id, str) and raw_turn_id:
                        current_turn_id = raw_turn_id
                        if pending_user:
                            text, timestamp, offset = pending_user
                            yield Turn(
                                role="user",
                                text=text,
                                timestamp=timestamp,
                                session_id=session_id,
                                turn_id=f"history:{session_id}:{offset}",
                                tool_calls=[],
                                line_offset=offset,
                            )
                            pending_user = None
                    continue

                if entry_type != "response_item" or not isinstance(payload, dict):
                    continue

                payload_type = payload.get("type")
                if payload_type == "function_call":
                    name = str(payload.get("name") or "unknown")
                    assistant_tool_calls.append({
                        "name": name,
                        "input": _summarize_tool_input(
                            name,
                            _parse_codex_arguments(payload.get("arguments", {})),
                        ),
                    })
                    continue

                if payload_type != "message":
                    continue

                role = payload.get("role")
                if role not in ("user", "assistant"):
                    continue
                text = _extract_text(payload.get("content", ""))
                if not text:
                    continue
                timestamp = entry.get("timestamp", "") or ""
                if role == "user":
                    pending_user = (text, timestamp, line_no)
                    continue
                turn_id = current_turn_id or f"history:{session_id}:{line_no}"
                yield Turn(
                    role="assistant",
                    text=text,
                    timestamp=timestamp,
                    session_id=session_id,
                    turn_id=turn_id,
                    tool_calls=assistant_tool_calls,
                    line_offset=line_no,
                )
                assistant_tool_calls = []

        if pending_user:
            text, timestamp, offset = pending_user
            yield Turn(
                role="user",
                text=text,
                timestamp=timestamp,
                session_id=session_id,
                turn_id=f"history:{session_id}:{offset}",
                tool_calls=[],
                line_offset=offset,
            )
    except OSError:
        return


def _sort_key(t: Turn) -> tuple[str, str, int]:
    return (t.timestamp, t.session_id, t.line_offset)


def _sort_newest_first(turns: list[Turn]) -> list[Turn]:
    turns.sort(key=_sort_key, reverse=True)
    return turns


def load_peer_history(
    peer_path: str | None,
    backend: str,
    metadata: dict[str, Any] | None = None,
) -> HistoryLoadResult:
    """Load and sort-merge local history with explicit backend capability state."""
    backend_value = getattr(backend, "value", backend)
    turns: list[Turn] = []
    if backend_value == "claude-code":
        for path in discover_claude_sessions(peer_path):
            turns.extend(_iter_claude_turns(path))
        return HistoryLoadResult(
            turns=_sort_newest_first(turns),
            status="available" if turns else "unavailable",
            backend=backend_value,
            message=(
                "Claude Code transcript history loaded."
                if turns else "No Claude Code transcript history found for this peer path."
            ),
        )

    if backend_value == "codex":
        history_backend = "codex-acp" if metadata and metadata.get("acp") else "codex"
        for path in discover_codex_sessions(peer_path):
            turns.extend(_iter_codex_turns(path))
        return HistoryLoadResult(
            turns=_sort_newest_first(turns),
            status="available" if turns else "unavailable",
            backend=history_backend,
            message=(
                f"{history_backend} rollout history loaded."
                if turns else f"No {history_backend} rollout history found for this peer path."
            ),
        )

    if backend_value in {"gemini", "opencode", "pi"}:
        return HistoryLoadResult(
            turns=[],
            status="unsupported",
            backend=backend_value,
            message=f"{backend_value} does not expose a supported local history source yet.",
        )

    return HistoryLoadResult(
        turns=[],
        status="unsupported",
        backend=backend_value,
        message=f"{backend_value} local history is not supported.",
    )


def load_peer_turns(peer_path: str | None, backend: str) -> list[Turn]:
    """Load local turns for compatibility with callers that only need turns.

    Newest-first. Sort key is the composite cursor tuple
    `(timestamp, session_id, line_offset)`; turns with empty timestamps sort to
    the end (oldest) in the newest-first ordering.
    """
    return load_peer_history(peer_path, backend).turns


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
