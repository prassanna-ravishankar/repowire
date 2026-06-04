"""Orchestrator workspace lifecycle: init, validate, update, backup.

The workspace at `~/.repowire/orchestrator/` is rendered from a bundled
template at `repowire/orchestrator/template/`. Init is atomic — if AGENTS.md
already exists, init is a no-op (use --force to re-render with backup).

Symlinks: CLAUDE.md points to AGENTS.md for Claude Code. Other supported
runtimes load AGENTS.md directly. Claude Code also gets `.claude/skills/*`
symlinks to the canonical `.agents/skills/*` skill directories.
"""

from __future__ import annotations

import logging
import shutil
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import datetime, timezone
from importlib import resources
from pathlib import Path

from repowire.config import paths as config_paths

logger = logging.getLogger(__name__)

# Runtime-native filenames that should symlink to AGENTS.md. AGENTS.md is the
# source of truth; Claude Code gets CLAUDE.md as a compatibility shim.
RUNTIME_SYMLINKS = ("CLAUDE.md",)

# Source-of-truth filename. Presence is the install marker for atomic detection.
SOURCE_FILE = "AGENTS.md"

SKILL_NAMES = (
    "adhd",
    "cleanup",
    "coordination",
    "create-agent",
    "create-skill",
    "delegation",
    "durable-jobs",
    "handover",
    "memory",
    "review-cycle",
    "worktree-isolation",
)

# Files repowire ships and may update across releases. update_workspace()
# offers per-file diff prompts for these.
REPOWIRE_OWNED_FILES = (
    "AGENTS.md",
    ".agents/skills/adhd/SKILL.md",
    ".agents/skills/cleanup/SKILL.md",
    ".agents/skills/coordination/SKILL.md",
    ".agents/skills/create-agent/SKILL.md",
    ".agents/skills/create-skill/SKILL.md",
    ".agents/skills/delegation/SKILL.md",
    ".agents/skills/durable-jobs/SKILL.md",
    ".agents/skills/handover/SKILL.md",
    ".agents/skills/memory/SKILL.md",
    ".agents/skills/review-cycle/SKILL.md",
    ".agents/skills/worktree-isolation/SKILL.md",
    "orchestrator.yaml.example",
)

# Files the orchestrator owns at runtime — never touched by update.
ORCHESTRATOR_OWNED_FILES = (
    "SOUL.md",
    "comms.md",
    "projects.md",
    "BOOTSTRAP.md",
    "memory/MEMORY.md",
)


def _claude_skill_link_name(name: str) -> str:
    return f".claude/skills/{name}"


def _canonical_skill_dir(ws: Path, name: str) -> Path:
    return ws / ".agents" / "skills" / name


def _backup_path(path: Path) -> Path:
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup = path.parent / f"{path.name}.bak.{timestamp}"
    counter = 1
    while backup.exists():
        backup = path.parent / f"{path.name}.bak.{timestamp}.{counter}"
        counter += 1
    shutil.move(str(path), str(backup))
    return backup


def _ensure_claude_skill_symlinks(ws: Path) -> None:
    """Expose canonical .agents skills through Claude Code's skill directory."""
    claude_skills = ws / ".claude" / "skills"
    claude_skills.mkdir(parents=True, exist_ok=True)
    for name in SKILL_NAMES:
        link = claude_skills / name
        if link.exists() or link.is_symlink():
            if link.is_dir() and not link.is_symlink():
                _backup_path(link)
            else:
                link.unlink()
        link.symlink_to(Path("..") / ".." / ".agents" / "skills" / name)


def _claude_skill_symlink_broken(ws: Path, name: str) -> bool:
    link = ws / ".claude" / "skills" / name
    expected = _canonical_skill_dir(ws, name)
    return not link.is_symlink() or link.resolve() != expected.resolve()


def workspace_path() -> Path:
    """Return the orchestrator workspace path."""
    return config_paths.get_config_dir() / "orchestrator"


@contextmanager
def _template_root() -> Iterator[Path]:
    """Yield the bundled template root as a real filesystem Path.

    Uses importlib.resources.as_file() so the template works whether the
    package is installed as a wheel (extracted to site-packages), an editable
    install (source-tree path), or a zip-imported wheel (extracted to a temp
    directory for the duration of the context). The yielded path is only
    valid inside the `with` block.
    """
    traversable = resources.files("repowire.orchestrator") / "template"
    with resources.as_file(traversable) as root:
        yield root


def is_installed() -> bool:
    """Atomic install detection: presence of AGENTS.md."""
    return (workspace_path() / SOURCE_FILE).exists()


def init_workspace(*, force: bool = False) -> tuple[bool, str]:
    """Render the bundled template into the workspace.

    Atomic semantics: if AGENTS.md already exists and force=False, this is
    a no-op. With force=True, the existing workspace is moved to a
    timestamped backup before re-rendering.

    Returns (rendered, message). `rendered=False` when no-op.
    """
    ws = workspace_path()
    if is_installed() and not force:
        return False, (
            f"Orchestrator already installed at {ws}. "
            "Use --force to reinstall (current workspace backed up)."
        )

    if is_installed() and force:
        backup = backup_workspace()
        logger.info("Backed up existing workspace to %s", backup)
        # Remove the workspace itself before re-rendering so we start fresh
        if ws.exists():
            shutil.rmtree(ws)

    with _template_root() as template_root:
        if not template_root.is_dir():
            return False, f"Bundled template not found at {template_root}"

        # Copy the template tree. dirs_exist_ok=True so an empty
        # ~/.repowire/orchestrator/ (user-mkdir'd, or leftover scaffolding)
        # doesn't trip FileExistsError — we only fail-fast when AGENTS.md is
        # present (atomic-install marker check above).
        shutil.copytree(template_root, ws, dirs_exist_ok=True)

    # Create runtime-native symlinks pointing at AGENTS.md
    source = ws / SOURCE_FILE
    if not source.exists():
        return False, f"Template did not produce {SOURCE_FILE} (broken template?)"
    for name in RUNTIME_SYMLINKS:
        link = ws / name
        if link.exists() or link.is_symlink():
            link.unlink()
        # Relative symlink — workspace is portable across mounts
        link.symlink_to(SOURCE_FILE)

    _ensure_claude_skill_symlinks(ws)

    return True, f"Orchestrator workspace created at {ws}"


def backup_workspace() -> Path:
    """Move the current workspace to a timestamped .bak sibling.

    Returns the backup path. Raises if no workspace exists.
    """
    ws = workspace_path()
    if not ws.exists():
        raise FileNotFoundError(f"No workspace to back up at {ws}")
    return _backup_path(ws)


def validate_workspace() -> tuple[bool, list[str]]:
    """Preflight check before `orchestrator start`. Returns (ok, errors)."""
    errors: list[str] = []
    ws = workspace_path()
    if not ws.is_dir():
        errors.append(f"Workspace directory missing: {ws}")
        return False, errors

    source = ws / SOURCE_FILE
    if not source.exists():
        errors.append(f"Source file missing: {source}")
    soul = ws / "SOUL.md"
    if not soul.exists():
        errors.append(f"Persona shim missing: {soul}")

    for name in RUNTIME_SYMLINKS:
        link = ws / name
        if not link.is_symlink():
            errors.append(f"Expected symlink missing or not a symlink: {link}")
            continue
        target = link.resolve()
        if target != source.resolve():
            errors.append(
                f"Symlink {link} points to {target}, expected {source.resolve()}"
            )

    for name in SKILL_NAMES:
        skill = _canonical_skill_dir(ws, name) / "SKILL.md"
        if not skill.is_file():
            errors.append(f"Skill file missing: {skill}")
        if _claude_skill_symlink_broken(ws, name):
            errors.append(
                f"Expected Claude skill symlink missing or broken: "
                f"{ws / _claude_skill_link_name(name)}"
            )

    return len(errors) == 0, errors


def update_workspace() -> list[tuple[str, str]]:
    """Compare shipped templates vs. workspace files. Return [(path, status)].

    Status is one of:
      - 'identical' — workspace matches shipped, no action
      - 'differs'   — workspace differs from shipped; caller should diff/prompt
      - 'missing'   — workspace file absent; caller should restore
      - 'symlink-broken' — symlink missing or wrong target; caller should fix

    Files in ORCHESTRATOR_OWNED_FILES are skipped (never reported as differing).
    """
    ws = workspace_path()
    report: list[tuple[str, str]] = []

    with _template_root() as template_root:
        for rel in REPOWIRE_OWNED_FILES:
            shipped = template_root / rel
            local = ws / rel
            if not shipped.exists():
                continue  # template no longer ships this file
            if not local.exists():
                report.append((rel, "missing"))
                continue
            if shipped.read_bytes() == local.read_bytes():
                report.append((rel, "identical"))
            else:
                report.append((rel, "differs"))

    # Symlinks
    source = ws / SOURCE_FILE
    for name in RUNTIME_SYMLINKS:
        link = ws / name
        if not link.is_symlink() or link.resolve() != source.resolve():
            report.append((name, "symlink-broken"))

    for name in SKILL_NAMES:
        if _claude_skill_symlink_broken(ws, name):
            report.append((_claude_skill_link_name(name), "symlink-broken"))

    return report
