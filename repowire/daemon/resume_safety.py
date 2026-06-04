"""Shared resume-safety decision for restart, jobs, and session control.

Three pathways need the same question answered before resuming a backend
session: restart (`routes/spawn.py`), durable/recurring jobs
(`job_runner`), and session control (`session_control`). Previously each
hand-rolled its own resume-plan logic and only restart pre-validated the
session id against disk -- the others would attach a resume plan for a stale
id that fails hard at launch.

This module is the single resume-safety seam: given a backend + path +
captured runtime_session_id (+ optional capability hint), it decides whether
resuming is safe and returns a plan, or refuses with a specific reason. Both
Claude and Codex exit non-zero on an unknown id (no fresh fallback), so an
id whose session file is gone -- or a backend whose storage we cannot scan --
must NOT be resumed.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from repowire.agent_backends import AgentResumePlan, agent_backend_for
from repowire.agent_types import AgentType

# Status values (documented set): "resumable", "missing_id", "unsupported",
# "unvalidated_backend", "stale_missing_file". Kept as str to avoid Literal
# narrowing friction at the history.py boundary.


@dataclass(frozen=True)
class ResumeSafetyDecision:
    """Whether a backend session can be safely resumed, and why/why not."""

    plan: AgentResumePlan | None
    resumable: bool
    validation_status: str
    warning: str | None


def _warning_for(status: str, backend_value: str) -> str | None:
    if status == "resumable":
        return None
    if status == "missing_id":
        return "no captured backend session id for this peer"
    if status == "unsupported":
        return f"backend {backend_value} does not support resume"
    if status == "unvalidated_backend":
        return (
            f"backend {backend_value} supports resume but Repowire cannot yet "
            "pre-validate its session store; restarting fresh to avoid launching "
            "a dead resumed runtime"
        )
    if status == "stale_missing_file":
        return (
            "captured session id has no local session file (stale/expired); "
            "resuming would fail, so restarting fresh"
        )
    return None


def resolve_resume_safety(
    *,
    backend: AgentType,
    path: str | None,
    runtime_session_id: str | None,
    repowire_session_id: str | None = None,
    capability: dict[str, Any] | None = None,
) -> ResumeSafetyDecision:
    """Decide whether ``runtime_session_id`` can be safely resumed for ``backend``.

    Returns a plan only when a local session file for exactly this id exists on
    disk (for backends whose storage we can scan). For unmapped backends, or a
    stale id, returns no plan plus a specific warning so callers can fall back
    to a fresh launch without first killing a live pane.
    """
    backend_value = getattr(backend, "value", backend)

    if not runtime_session_id:
        return ResumeSafetyDecision(None, False, "missing_id",
                                    _warning_for("missing_id", backend_value))

    # Caller-carried capability hint (e.g. job binding recorded supported=False).
    if isinstance(capability, dict) and capability.get("supported") is False:
        return ResumeSafetyDecision(None, False, "unsupported",
                                    _warning_for("unsupported", backend_value))

    backend_obj = agent_backend_for(backend)
    if not backend_obj.can_resume(runtime_session_id):
        return ResumeSafetyDecision(None, False, "unsupported",
                                    _warning_for("unsupported", backend_value))

    status = backend_obj.runtime_session_validation_status(path, runtime_session_id)
    if status != "resumable":
        return ResumeSafetyDecision(None, False, status, _warning_for(status, backend_value))

    plan = AgentResumePlan(
        backend=backend,
        runtime_session_id=runtime_session_id,
        repowire_session_id=repowire_session_id,
        capability=capability if isinstance(capability, dict) else {},
    )
    return ResumeSafetyDecision(plan, True, "resumable", None)
