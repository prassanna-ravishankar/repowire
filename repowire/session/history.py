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
from urllib.parse import unquote, urlparse

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


def _path_from_source_uri(runtime_source_uri: str | None, backend: str) -> Path | None:
    """Resolve known runtime source locators to local backend-owned files.

    Source URIs remain opaque at the route boundary. This helper only teaches
    history replay about the locators documented in the session binding
    contract, plus file/plain-path forms used by tests and local adapters.
    """
    if not runtime_source_uri:
        return None
    backend_value = getattr(backend, "value", backend)
    parsed = urlparse(runtime_source_uri)
    if parsed.scheme == "claude-jsonl":
        return _claude_projects_dir() / unquote(parsed.path.lstrip("/"))
    if parsed.scheme == "codex-rollout":
        return _codex_sessions_dir() / unquote(parsed.path.lstrip("/"))
    if parsed.scheme == "file":
        return Path(unquote(parsed.path)).expanduser()
    if not parsed.scheme:
        return Path(runtime_source_uri).expanduser()

    # Compatibility for source locators that encode only backend type in the
    # scheme but still carry a path payload.
    if backend_value == "claude-code" and parsed.scheme.startswith("claude"):
        return _claude_projects_dir() / unquote(parsed.path.lstrip("/"))
    if backend_value == "codex" and parsed.scheme.startswith("codex"):
        return _codex_sessions_dir() / unquote(parsed.path.lstrip("/"))
    return None


def _session_file_candidates(
    peer_path: str | None,
    backend: str,
    runtime_session_id: str | None,
) -> list[Path]:
    if not runtime_session_id:
        return []
    backend_value = getattr(backend, "value", backend)
    if backend_value == "claude-code" and peer_path:
        direct = _claude_projects_dir() / _encode_cwd(peer_path) / f"{runtime_session_id}.jsonl"
        discovered = discover_claude_sessions(peer_path)
        return [direct, *[path for path in discovered if path != direct]]
    if backend_value == "codex":
        return discover_codex_sessions(peer_path)
    return []


def _opencode_storage_dir() -> Path:
    return Path.home() / ".local" / "share" / "opencode" / "storage" / "session"


def _antigravity_cli_dir() -> Path:
    return Path.home() / ".gemini" / "antigravity-cli"


def _gemini_tmp_dir() -> Path:
    return Path.home() / ".gemini" / "tmp"


def _pi_session_map_path() -> Path:
    return Path.home() / ".pi" / "pi-acp" / "session-map.json"


def _claude_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    # The id is embedded in the filename; require an exact stem match so a stale
    # id + another session in the same cwd cannot false-positive.
    for path in _session_file_candidates(peer_path, "claude-code", runtime_session_id):
        if path.stem == runtime_session_id and _safe_is_file(path):
            return True
    return False


def _codex_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    for path in _session_file_candidates(peer_path, "codex", runtime_session_id):
        if runtime_session_id in path.name and _safe_is_file(path):
            return True
    return False


def _opencode_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    # Sessions: ~/.local/share/opencode/storage/session/<projectID>/ses_<id>.json
    # with JSON fields id + directory. The projectID dir is a hash (not derivable
    # from cwd), so scan and match the JSON id + directory rather than the path.
    root = _opencode_storage_dir()
    if not root.is_dir():
        return False
    targets = {runtime_session_id, f"ses_{runtime_session_id}"}
    try:
        for path in root.glob("*/ses_*.json"):
            try:
                data = json.loads(path.read_text())
            except (OSError, json.JSONDecodeError):
                continue
            if not isinstance(data, dict) or data.get("id") not in targets:
                continue
            if peer_path is None or _path_matches(data.get("directory"), peer_path):
                return True
    except OSError:
        return False
    return False


def _pi_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    # ~/.pi/pi-acp/session-map.json: {"sessions": {"<id>": {sessionId, cwd,
    # sessionFile, ...}}}. Require the mapped cwd to match and the sessionFile
    # to exist.
    try:
        data = json.loads(_pi_session_map_path().read_text())
    except (OSError, json.JSONDecodeError):
        return False
    entry = (data.get("sessions") or {}).get(runtime_session_id) if isinstance(data, dict) else None
    if not isinstance(entry, dict):
        return False
    if peer_path is not None and not _path_matches(entry.get("cwd"), peer_path):
        return False
    session_file = entry.get("sessionFile")
    return isinstance(session_file, str) and _safe_is_file(Path(session_file))


def _antigravity_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    # ~/.gemini/antigravity-cli/cache/last_conversations.json maps cwd -> id,
    # with conversations/<id>.pb. Conservative: only validates the LAST
    # conversation per cwd, so older ids false-negative to fresh+warning (safe;
    # never false-positive). cwd must map to exactly this id AND the .pb exist.
    cli = _antigravity_cli_dir()
    try:
        last = json.loads((cli / "cache" / "last_conversations.json").read_text())
    except (OSError, json.JSONDecodeError):
        return False
    if not isinstance(last, dict):
        return False
    if peer_path is not None:
        mapped = next(
            (cid for cwd, cid in last.items() if _path_matches(cwd, peer_path)), None
        )
        if mapped != runtime_session_id:
            return False
    elif runtime_session_id not in last.values():
        return False
    return _safe_is_file(cli / "conversations" / f"{runtime_session_id}.pb")


def _gemini_resumable(peer_path: str | None, runtime_session_id: str) -> bool:
    # gemini --resume accepts a full session UUID (per official docs). Sessions:
    # ~/.gemini/tmp/<projectHash>/chats/session-<ts>-<shortid>.json with JSON
    # fields sessionId (UUID) + projectHash. The dir is a hash (not derivable
    # from cwd), so scan and match sessionId; scope by directory when the JSON
    # records it (older sessions may not), else accept the sessionId match.
    root = _gemini_tmp_dir()
    if not root.is_dir():
        return False
    try:
        for path in root.glob("*/chats/session-*.json"):
            try:
                data = json.loads(path.read_text())
            except (OSError, json.JSONDecodeError):
                continue
            if not isinstance(data, dict) or data.get("sessionId") != runtime_session_id:
                continue
            directory = data.get("directory") or data.get("cwd")
            if peer_path is None or directory is None or _path_matches(directory, peer_path):
                return True
    except OSError:
        return False
    return False


def _safe_is_file(path: Path) -> bool:
    try:
        return path.expanduser().is_file()
    except OSError:
        return False


# Per-backend on-disk resume validators. Backends absent from this table declare
# a resume invocation but have no mapped session store, so their status is
# "unvalidated_backend" (the seam falls back to fresh+warning rather than risk a
# resume that exits hard after the pane is already killed). Gemini is currently
# unmapped (no clear per-id session contract found locally).
_RESUME_VALIDATORS = {
    "claude-code": _claude_resumable,
    "codex": _codex_resumable,
    "opencode": _opencode_resumable,
    "pi": _pi_resumable,
    "antigravity": _antigravity_resumable,
    "gemini": _gemini_resumable,
}

PREVALIDATABLE_RESUME_BACKENDS = frozenset(_RESUME_VALIDATORS)


def runtime_session_validation_status(
    peer_path: str | None,
    backend: str,
    runtime_session_id: str | None,
) -> str:
    """Granular pre-validation status for a runtime_session_id.

    Returns one of:
    - "missing_id": no id captured
    - "unvalidated_backend": backend resume storage not mapped (can't prove)
    - "stale_missing_file": known backend, but no local session for the id
    - "resumable": a local session for exactly this id exists

    Resume-capable backends exit non-zero on an unknown id (no fresh fallback),
    so a resume that kills the pane first would leave the peer dead. Each mapped
    backend's validator proves the id maps to a real local session scoped to the
    peer's cwd; unmapped backends stay "unvalidated_backend" so callers fall back
    to a fresh launch instead of guessing.
    """
    if not runtime_session_id:
        return "missing_id"
    backend_value = getattr(backend, "value", backend)
    validator = _RESUME_VALIDATORS.get(backend_value)
    if validator is None:
        return "unvalidated_backend"
    return "resumable" if validator(peer_path, runtime_session_id) else "stale_missing_file"


def runtime_session_resumable(
    peer_path: str | None,
    backend: str,
    runtime_session_id: str | None,
) -> bool:
    """Whether a local backend session file exists for ``runtime_session_id``.

    Thin bool wrapper over ``runtime_session_validation_status`` for callers
    that only need a yes/no. See that function for the per-status semantics.
    """
    return (
        runtime_session_validation_status(peer_path, backend, runtime_session_id)
        == "resumable"
    )


def _load_paths(
    paths: list[Path],
    backend: str,
    runtime_session_id: str | None,
) -> list[Turn]:
    backend_value = getattr(backend, "value", backend)
    turns: list[Turn] = []
    seen: set[Path] = set()
    for path in paths:
        try:
            resolved = path.expanduser().resolve()
        except OSError:
            resolved = path.expanduser()
        if resolved in seen or not resolved.is_file():
            continue
        seen.add(resolved)
        if backend_value == "claude-code":
            turns.extend(_iter_claude_turns(resolved))
        elif backend_value == "codex":
            turns.extend(_iter_codex_turns(resolved))
    if runtime_session_id:
        turns = [turn for turn in turns if turn.session_id == runtime_session_id]
    return turns


def load_bound_history(
    *,
    peer_path: str | None,
    backend: str,
    runtime_session_id: str | None,
    runtime_source_uri: str | None,
    metadata: dict[str, Any] | None = None,
) -> HistoryLoadResult:
    """Load history through a session binding's runtime source boundary."""
    backend_value = getattr(backend, "value", backend)
    history_backend = (
        "codex-acp"
        if backend_value == "codex" and metadata and metadata.get("acp")
        else backend_value
    )
    if backend_value not in {"claude-code", "codex"}:
        return HistoryLoadResult(
            turns=[],
            status="unsupported",
            backend=history_backend,
            message=f"{history_backend} local history is not supported.",
        )

    paths: list[Path] = []
    source_path = _path_from_source_uri(runtime_source_uri, backend_value)
    if source_path is not None:
        paths.append(source_path)
    paths.extend(_session_file_candidates(peer_path, backend_value, runtime_session_id))
    turns = _load_paths(paths, backend_value, runtime_session_id)

    if turns:
        return HistoryLoadResult(
            turns=_sort_newest_first(turns),
            status="available",
            backend=history_backend,
            message=f"{history_backend} history loaded from session binding.",
        )

    if runtime_source_uri or runtime_session_id:
        return HistoryLoadResult(
            turns=[],
            status="unavailable",
            backend=history_backend,
            message=f"No {history_backend} history found for this session binding.",
        )

    return load_peer_history(peer_path, backend_value, metadata)


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
