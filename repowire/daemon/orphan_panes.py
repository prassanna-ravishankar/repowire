"""Orphan-pane discovery: tmux panes running an agent the mesh never registered.

When hooks/MCP don't fire (seen during dogfood), a live agent pane is invisible
to the daemon. This surfaces those panes so a human can intentionally link them
(see ``repowire link``). Detection here is a display-only *hint* — never an
action. We deliberately list ALL unregistered panes (not just agent-looking
ones), because the weird cases this tool exists to diagnose are exactly the ones
a command-name filter would hide.
"""

from __future__ import annotations

from typing import TypedDict

from repowire.hooks._tmux import PaneInfo, list_all_panes

# Known agent command basenames → backend hint. Matched against a pane's
# current command only; this is a display hint, not an authoritative claim.
_AGENT_COMMANDS = {
    "claude": "claude-code",
    "codex": "codex",
    "gemini": "gemini",
    "opencode": "opencode",
    "antigravity": "antigravity",
    "pi": "pi",
}


class OrphanPane(TypedDict):
    """An unregistered tmux pane, with a best-effort backend hint."""

    pane_id: str
    pid: int
    command: str
    cwd: str
    session: str
    window: str
    detected_backend: str  # claude-code|codex|gemini|opencode|antigravity|pi|unknown
    confidence: str  # "hint" when the command matched a known agent, else "unknown"


def detect_backend(command: str) -> tuple[str, str]:
    """Best-effort (backend, confidence) from a pane command. Display hint only."""
    base = command.strip().rsplit("/", 1)[-1].lower()
    backend = _AGENT_COMMANDS.get(base)
    if backend is not None:
        return backend, "hint"
    return "unknown", "unknown"


def find_orphan_panes(registered_pane_ids: set[str]) -> list[OrphanPane]:
    """All tmux panes whose pane_id is not bound to a registered peer.

    No command filtering — every unregistered pane is returned, tagged with a
    detected-backend hint so a UI can group likely agents first without hiding
    the rest.
    """
    orphans: list[OrphanPane] = []
    for pane in list_all_panes():
        if pane["pane_id"] in registered_pane_ids:
            continue
        backend, confidence = detect_backend(pane["command"])
        orphans.append(_to_orphan(pane, backend, confidence))
    return orphans


def _to_orphan(pane: PaneInfo, backend: str, confidence: str) -> OrphanPane:
    return OrphanPane(
        pane_id=pane["pane_id"],
        pid=pane["pid"],
        command=pane["command"],
        cwd=pane["cwd"],
        session=pane["session"],
        window=pane["window"],
        detected_backend=backend,
        confidence=confidence,
    )
