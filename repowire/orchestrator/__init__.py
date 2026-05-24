"""Orchestrator workspace management.

The orchestrator is a productized version of a hand-rolled pattern — a
dedicated mesh peer that coordinates work across other peers. Its workspace
lives at `~/.repowire/orchestrator/` and is scaffolded from a bundled
template at `repowire/orchestrator/template/`.

See GitHub issue #38 and `/Users/prass/.claude/plans/plan-it-out-glittery-bentley.md`.
"""

from repowire.orchestrator.persona import (
    ActiveSoul,
    PersonaSummary,
    build_soul_context,
    clear_active_persona,
    find_soul_path,
    get_active_persona,
    list_personas,
    load_active_soul,
    load_soul,
    set_active_persona,
    validate_persona_name,
    write_soul,
)
from repowire.orchestrator.workspace import (
    backup_workspace,
    init_workspace,
    is_installed,
    update_workspace,
    validate_workspace,
    workspace_path,
)

__all__ = [
    "ActiveSoul",
    "PersonaSummary",
    "backup_workspace",
    "build_soul_context",
    "clear_active_persona",
    "find_soul_path",
    "get_active_persona",
    "init_workspace",
    "is_installed",
    "list_personas",
    "load_active_soul",
    "load_soul",
    "set_active_persona",
    "update_workspace",
    "validate_persona_name",
    "validate_workspace",
    "workspace_path",
    "write_soul",
]
