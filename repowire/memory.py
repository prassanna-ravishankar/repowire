"""Filesystem-backed mesh memory helpers.

This module owns only local, explicit memory operations. No hook, scheduler, or
daemon side effect calls into this layer to learn silently.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, cast

import yaml

from repowire.config import paths as config_paths
from repowire.orchestrator.persona import get_active_persona, validate_persona_name
from repowire.orchestrator.workspace import workspace_path

MEMORY_INDEX = "MEMORY.md"
_SAFE_NAME_RE = re.compile(r"^[a-zA-Z0-9._-]+$")
_SCOPE_ALIASES = {
    "project": "projects",
    "projects": "projects",
    "persona": "personas",
    "personas": "personas",
    "global": "global",
    "user": "user",
    "orchestrator": "orchestrator",
}


@dataclass(frozen=True)
class MemoryEntry:
    """A single memory file discovered in a resolved scope."""

    scope: str
    slug: str
    path: Path
    type: str
    description: str
    updated_at: str


@dataclass(frozen=True)
class MemorySearchHit:
    """A search match in a memory file."""

    scope: str
    slug: str
    path: Path
    line: int
    snippet: str


def validate_memory_name(name: str, *, label: str = "name") -> str:
    """Validate a memory slug, project name, or scope path segment."""
    cleaned = name.strip()
    if not _SAFE_NAME_RE.fullmatch(cleaned):
        raise ValueError(f"{label} must match ^[a-zA-Z0-9._-]+$")
    return cleaned


def memory_root() -> Path:
    """Return the mesh-memory root directory."""
    return config_paths.get_config_dir() / "memory"


def normalize_scope(scope: str | None) -> str:
    """Return a canonical mesh-memory scope."""
    if scope is None:
        return default_scope()
    cleaned = scope.strip().lower()
    try:
        return _SCOPE_ALIASES[cleaned]
    except KeyError as e:
        raise ValueError(
            "scope must be one of: global, user, project, projects, "
            "persona, personas, orchestrator"
        ) from e


def default_scope() -> str:
    """Resolve the local default scope without consulting the daemon."""
    try:
        cwd = Path.cwd().resolve()
        orchestrator = workspace_path().resolve()
        if cwd == orchestrator or orchestrator in cwd.parents:
            return "orchestrator"
    except OSError:
        pass
    return "user"


def resolve_scope_path(
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
) -> tuple[str, Path]:
    """Resolve a mesh-memory scope to its label and directory path."""
    canonical = normalize_scope(scope)
    root = memory_root()
    if canonical == "projects":
        if project is None:
            project = Path.cwd().name
        name = validate_memory_name(project, label="project")
        return f"projects/{name}", root / "projects" / name
    if canonical == "personas":
        name = persona or get_active_persona()
        if name is None:
            raise ValueError("persona scope requires --persona or an active persona")
        safe_name = validate_persona_name(name)
        return f"personas/{safe_name}", root / "personas" / safe_name
    if canonical == "orchestrator":
        return "orchestrator", workspace_path() / "memory"
    return canonical, root / canonical


def iter_scope_paths(
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
    all_scopes: bool = False,
) -> list[tuple[str, Path]]:
    """Return scope paths for a targeted command or an all-scope search."""
    if not all_scopes:
        return [resolve_scope_path(scope, project=project, persona=persona)]

    root = memory_root()
    paths: list[tuple[str, Path]] = [
        ("global", root / "global"),
        ("user", root / "user"),
        ("orchestrator", workspace_path() / "memory"),
    ]
    for parent, prefix in ((root / "projects", "projects"), (root / "personas", "personas")):
        if not parent.is_dir():
            continue
        for child in sorted(parent.iterdir(), key=lambda p: p.name):
            if child.is_dir() or child.is_symlink():
                try:
                    validate_memory_name(child.name, label=prefix[:-1])
                except ValueError:
                    continue
                paths.append((f"{prefix}/{child.name}", child))
    return paths


def _memory_files(root: Path) -> list[Path]:
    if not root.is_dir():
        return []
    return sorted(
        path
        for path in root.glob("*.md")
        if path.name != MEMORY_INDEX and path.is_file()
    )


def _split_frontmatter(content: str) -> tuple[dict[str, Any], str]:
    if not content.startswith("---\n"):
        return {}, content
    marker = "\n---\n"
    end = content.find(marker, 4)
    if end == -1:
        return {}, content
    raw = content[4:end]
    body = content[end + len(marker) :]
    try:
        parsed = yaml.safe_load(raw) or {}
    except yaml.YAMLError:
        return {}, body
    if not isinstance(parsed, dict):
        return {}, body
    return parsed, body


def _entry_from_file(scope: str, path: Path) -> MemoryEntry:
    content = path.read_text(encoding="utf-8")
    meta, _body = _split_frontmatter(content)
    raw_metadata = meta.get("metadata")
    metadata = cast(dict[str, Any], raw_metadata) if isinstance(raw_metadata, dict) else {}
    slug = str(meta.get("name") or path.stem)
    return MemoryEntry(
        scope=scope,
        slug=slug,
        path=path,
        type=str(metadata.get("type") or ""),
        description=str(meta.get("description") or ""),
        updated_at=str(metadata.get("updated_at") or _mtime_iso(path)),
    )


def list_memories(
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
) -> list[MemoryEntry]:
    """List memory entries in a scope. Missing scopes are empty."""
    label, root = resolve_scope_path(scope, project=project, persona=persona)
    return [_entry_from_file(label, path) for path in _memory_files(root)]


def read_memory(
    slug: str,
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
) -> tuple[str, Path]:
    """Read a memory file by slug and return its full Markdown content."""
    safe_slug = validate_memory_name(slug, label="slug")
    _label, root = resolve_scope_path(scope, project=project, persona=persona)
    path = root / f"{safe_slug}.md"
    if not path.is_file():
        raise FileNotFoundError(path)
    return path.read_text(encoding="utf-8"), path


def write_memory(
    slug: str,
    body: str,
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
    memory_type: str = "reference",
    description: str = "",
    append: bool = False,
    overwrite: bool = False,
) -> Path:
    """Write one explicit memory file and refresh the scope index."""
    if append and overwrite:
        raise ValueError("append and overwrite are mutually exclusive")
    safe_slug = validate_memory_name(slug, label="slug")
    safe_type = validate_memory_name(memory_type, label="type")
    label, root = resolve_scope_path(scope, project=project, persona=persona)
    root.mkdir(parents=True, exist_ok=True)
    if label == "orchestrator":
        _ensure_orchestrator_link(root)

    path = root / f"{safe_slug}.md"
    if path.exists() and not (append or overwrite):
        raise FileExistsError(path)

    clean_body = body.strip()
    if append and path.exists():
        existing = path.read_text(encoding="utf-8")
        _meta, existing_body = _split_frontmatter(existing)
        clean_body = f"{existing_body.strip()}\n\n{clean_body}".strip()

    path.write_text(
        _render_memory(
            safe_slug,
            clean_body,
            memory_type=safe_type,
            description=description.strip(),
        ),
        encoding="utf-8",
    )
    _refresh_index(label, root)
    return path


def search_memories(
    query: str,
    scope: str | None = None,
    *,
    project: str | None = None,
    persona: str | None = None,
    all_scopes: bool = False,
) -> list[MemorySearchHit]:
    """Search memory Markdown files with case-insensitive substring matching."""
    needle = query.strip()
    if not needle:
        raise ValueError("query must not be empty")
    lowered = needle.lower()
    hits: list[MemorySearchHit] = []
    for label, root in iter_scope_paths(
        scope, project=project, persona=persona, all_scopes=all_scopes
    ):
        for path in _memory_files(root):
            slug = path.stem
            for line_no, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
                if lowered in line.lower():
                    hits.append(
                        MemorySearchHit(
                            scope=label,
                            slug=slug,
                            path=path,
                            line=line_no,
                            snippet=line.strip(),
                        )
                    )
                    break
    return hits


def _mtime_iso(path: Path) -> str:
    return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc).isoformat(
        timespec="seconds"
    )


def _render_memory(
    slug: str,
    body: str,
    *,
    memory_type: str,
    description: str,
) -> str:
    title = slug.replace("-", " ").replace("_", " ").title()
    if not body.startswith("# "):
        body = f"# {title}\n\n{body}".strip()
    frontmatter = yaml.safe_dump(
        {
            "name": slug,
            "description": description,
            "metadata": {
                "type": memory_type,
                "updated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            },
        },
        sort_keys=False,
    ).strip()
    return f"---\n{frontmatter}\n---\n\n{body}\n"


def _refresh_index(scope: str, root: Path) -> None:
    entries = [_entry_from_file(scope, path) for path in _memory_files(root)]
    lines = ["# Memory", ""]
    for entry in entries:
        memory_type = f" ({entry.type})" if entry.type else ""
        description = f" - {entry.description}" if entry.description else ""
        lines.append(f"- [{entry.slug}]({entry.slug}.md){memory_type}{description}")
    (root / MEMORY_INDEX).write_text("\n".join(lines).rstrip() + "\n", encoding="utf-8")


def _ensure_orchestrator_link(target: Path) -> None:
    link = memory_root() / "orchestrator"
    link.parent.mkdir(parents=True, exist_ok=True)
    if link.exists() or link.is_symlink():
        return
    try:
        link.symlink_to(target, target_is_directory=True)
    except OSError:
        pass
