"""Persona SOUL.md support for orchestrator workspaces."""

from __future__ import annotations

import hashlib
import os
import re
from dataclasses import dataclass
from pathlib import Path

from repowire.config import paths as config_paths
from repowire.orchestrator.workspace import workspace_path

_PERSONA_RE = re.compile(r"^[a-zA-Z0-9._-]+$")
ACTIVE_PERSONA_FILE = "ACTIVE_PERSONA"
SOUL_FILE = "SOUL.md"
SOUL_SHIM_FILE = "SOUL.md"
INACTIVE_SOUL_SHIM = (
    "# No Active Persona\n\n"
    "No orchestrator persona is active. Run "
    "`repowire orchestrator persona use <name>` to point this shim at a "
    "persona SOUL.md file.\n"
)


@dataclass(frozen=True)
class ActiveSoul:
    """Resolved persona identity loaded from a SOUL.md file."""

    name: str
    path: Path
    sha256: str
    content: str
    source: str

    @property
    def short_hash(self) -> str:
        return self.sha256[:12]


@dataclass(frozen=True)
class PersonaSummary:
    """Metadata for a discoverable persona SOUL.md."""

    name: str
    path: Path
    source: str
    sha256: str
    active: bool = False

    @property
    def short_hash(self) -> str:
        return self.sha256[:12]


def validate_persona_name(name: str) -> str:
    """Validate a persona name that maps to a local directory."""
    cleaned = name.strip()
    if not _PERSONA_RE.fullmatch(cleaned):
        raise ValueError("Persona name must match ^[a-zA-Z0-9._-]+$")
    return cleaned


def active_persona_path() -> Path:
    """Return the workspace-local active persona marker path."""
    return workspace_path() / "personas" / ACTIVE_PERSONA_FILE


def active_soul_shim_path() -> Path:
    """Return the stable workspace SOUL.md shim path."""
    return workspace_path() / SOUL_SHIM_FILE


def global_persona_dir(name: str) -> Path:
    """Return ~/.repowire/personas/<name>/."""
    return config_paths.get_config_dir() / "personas" / validate_persona_name(name)


def workspace_persona_dir(name: str) -> Path:
    """Return ~/.repowire/orchestrator/personas/<name>/."""
    return workspace_path() / "personas" / validate_persona_name(name)


def default_soul_path(name: str) -> Path:
    """Default write target for reusable personas."""
    return global_persona_dir(name) / SOUL_FILE


def workspace_soul_path(name: str) -> Path:
    """Workspace-local override target."""
    return workspace_persona_dir(name) / SOUL_FILE


def find_soul_path(name: str) -> tuple[Path, str] | tuple[None, None]:
    """Resolve a SOUL.md, preferring workspace-local over global persona."""
    local = workspace_soul_path(name)
    if local.is_file():
        return local, "workspace"
    global_path = default_soul_path(name)
    if global_path.is_file():
        return global_path, "global"
    return None, None


def _hash_content(content: str) -> str:
    return hashlib.sha256(content.encode()).hexdigest()


def load_soul(name: str) -> ActiveSoul | None:
    """Load active SOUL.md content and hash for a persona."""
    path, source = find_soul_path(name)
    if path is None or source is None:
        return None
    content = path.read_text(encoding="utf-8")
    digest = _hash_content(content)
    return ActiveSoul(
        name=validate_persona_name(name),
        path=path,
        sha256=digest,
        content=content,
        source=source,
    )


def get_active_persona() -> str | None:
    """Return the workspace active persona name, if configured."""
    marker = active_persona_path()
    if not marker.is_file():
        return None
    raw = marker.read_text(encoding="utf-8").strip()
    if not raw:
        return None
    return validate_persona_name(raw)


def set_active_persona(name: str) -> ActiveSoul:
    """Set the workspace active persona after verifying its SOUL.md exists."""
    soul = load_soul(name)
    if soul is None:
        raise FileNotFoundError(f"No SOUL.md found for persona {name!r}")
    marker = active_persona_path()
    marker.parent.mkdir(parents=True, exist_ok=True)
    marker.write_text(f"{soul.name}\n", encoding="utf-8")
    sync_active_soul_shim(soul)
    return soul


def clear_active_persona() -> bool:
    """Remove the active persona marker. Returns True if a marker was cleared."""
    marker = active_persona_path()
    if not marker.exists():
        sync_active_soul_shim(None)
        return False
    marker.unlink()
    sync_active_soul_shim(None)
    return True


def load_active_soul() -> ActiveSoul | None:
    """Load the currently active workspace persona, if configured."""
    name = get_active_persona()
    if name is None:
        return None
    return load_soul(name)


def sync_active_soul_shim(soul: ActiveSoul | None = None) -> Path:
    """Point workspace/SOUL.md at the active persona, or restore placeholder.

    AGENTS.md references this stable path. The active persona may live in a
    workspace override or the global persona directory, so the shim is the
    durable in-workspace target for runtimes that expand @SOUL.md.
    """
    path = active_soul_shim_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() or path.is_symlink():
        path.unlink()
    if soul is None:
        path.write_text(INACTIVE_SOUL_SHIM, encoding="utf-8")
        return path

    target = os.path.relpath(soul.path, start=path.parent)
    path.symlink_to(target)
    return path


def write_soul(
    name: str, content: str, *, workspace: bool = False, overwrite: bool = False
) -> Path:
    """Write SOUL.md for a persona and return the path."""
    path = workspace_soul_path(name) if workspace else default_soul_path(name)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() and not overwrite:
        raise FileExistsError(path)
    path.write_text(content, encoding="utf-8")
    return path


def list_personas() -> list[PersonaSummary]:
    """List personas discovered in workspace and global persona directories."""
    found: dict[tuple[str, str], PersonaSummary] = {}
    active = get_active_persona()
    active_path = find_soul_path(active)[0] if active else None
    for source, root in (
        ("workspace", workspace_path() / "personas"),
        ("global", config_paths.get_config_dir() / "personas"),
    ):
        if not root.is_dir():
            continue
        for soul_path in root.glob(f"*/{SOUL_FILE}"):
            name = soul_path.parent.name
            try:
                validate_persona_name(name)
            except ValueError:
                continue
            content = soul_path.read_text(encoding="utf-8")
            found[(source, name)] = PersonaSummary(
                name=name,
                path=soul_path,
                source=source,
                sha256=_hash_content(content),
                active=active_path is not None and soul_path == active_path,
            )
    return sorted(found.values(), key=lambda item: (item.name, item.source))


__all__ = [
    "ACTIVE_PERSONA_FILE",
    "INACTIVE_SOUL_SHIM",
    "SOUL_FILE",
    "SOUL_SHIM_FILE",
    "ActiveSoul",
    "PersonaSummary",
    "active_persona_path",
    "active_soul_shim_path",
    "clear_active_persona",
    "default_soul_path",
    "find_soul_path",
    "get_active_persona",
    "global_persona_dir",
    "list_personas",
    "load_active_soul",
    "load_soul",
    "set_active_persona",
    "sync_active_soul_shim",
    "validate_persona_name",
    "workspace_persona_dir",
    "workspace_soul_path",
    "write_soul",
]
