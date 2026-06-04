"""Shared backend-native session resume service."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

from repowire.agent_backends import AgentResumePlan, resume_capability_for_registration
from repowire.agent_types import AgentType
from repowire.daemon.resume_safety import resolve_resume_safety
from repowire.daemon.session_controls import SessionResolution
from repowire.daemon.spawn_service import SpawnService
from repowire.daemon.state.session_bindings import SessionBinding
from repowire.protocol.peers import PeerRole

ResumeServiceStatus = Literal["resume_available", "active_executor", "unsupported"]


class ResumeUnavailableError(RuntimeError):
    """Raised when a session cannot be safely resumed."""

    def __init__(
        self,
        *,
        error: str,
        message: str,
        status: ResumeServiceStatus = "unsupported",
        capability: str = "unavailable",
        warning: str | None = None,
    ) -> None:
        super().__init__(message)
        self.error = error
        self.message = message
        self.status = status
        self.capability = capability
        self.warning = warning


@dataclass(frozen=True)
class ResumeTarget:
    """Input needed to validate and optionally spawn a backend resume."""

    repowire_session_id: str | None
    backend: AgentType
    project_path: str
    runtime_session_id: str | None
    resume_capability: dict = field(default_factory=dict)
    status: str = "detached"
    peer_id: str | None = None
    executor_peer_id: str | None = None
    executor_peer_name: str | None = None
    runtime_source_uri: str | None = None

    @classmethod
    def from_binding(cls, binding: SessionBinding) -> ResumeTarget:
        return cls(
            repowire_session_id=binding.repowire_session_id,
            backend=AgentType(binding.backend),
            project_path=binding.project_path,
            runtime_session_id=binding.runtime_session_id,
            resume_capability=dict(binding.resume_capability),
            status=binding.status,
            peer_id=binding.peer_id,
            executor_peer_id=binding.current_executor_peer_id or binding.peer_id,
            runtime_source_uri=binding.runtime_source_uri,
        )


@dataclass(frozen=True)
class ResumeServiceResult:
    """Validated or executed resume result shared by HTTP routes and restart."""

    status: ResumeServiceStatus
    capability: Literal["supported"]
    message: str
    target: ResumeTarget
    resume_plan: AgentResumePlan
    resume_capability: dict
    action: Literal["inspect", "spawned"] = "inspect"
    command: str | None = None
    spawned_display_name: str | None = None
    tmux_session: str | None = None
    pane_id: str | None = None


def target_from_resolution(resolution: SessionResolution) -> ResumeTarget:
    """Build a resume target from a resolved durable session binding."""
    if resolution.has_active_executor:
        raise ResumeUnavailableError(
            error="active_executor",
            status="active_executor",
            capability="active_executor",
            message="Session already has an active executor; live sessions cannot be resumed.",
        )
    try:
        return ResumeTarget.from_binding(resolution.binding)
    except ValueError as exc:
        raise ResumeUnavailableError(
            error="unknown_backend",
            capability="unsupported",
            message=f"Unknown backend for session binding: {resolution.binding.backend}",
        ) from exc


def validate_resume_target(target: ResumeTarget) -> tuple[AgentResumePlan, dict]:
    """Validate backend support and local session storage for a resume target."""
    if target.status in {"archived", "lost", "superseded"}:
        raise ResumeUnavailableError(
            error="resume_unavailable",
            message=f"Session binding status is {target.status}; resume is not available.",
            warning=f"Session binding status is {target.status}.",
        )

    capability = dict(target.resume_capability)
    if target.runtime_session_id and not capability:
        capability = resume_capability_for_registration(target.backend, target.runtime_session_id)

    decision = resolve_resume_safety(
        backend=target.backend,
        path=target.project_path,
        runtime_session_id=target.runtime_session_id,
        repowire_session_id=target.repowire_session_id,
        capability=capability,
    )
    if not decision.resumable or decision.plan is None:
        raise ResumeUnavailableError(
            error="resume_unavailable",
            message=decision.warning or "Resume is not available for this session.",
            warning=decision.warning,
            capability="unsupported" if target.runtime_session_id else "unavailable",
        )
    return decision.plan, capability


def resume_target(
    *,
    target: ResumeTarget,
    spawn_service: SpawnService | None,
    dry_run: bool,
    profile: str | None = None,
    message: str | None = None,
    circle: str = "default",
    role: PeerRole = PeerRole.AGENT,
    peer_id: str | None = None,
) -> ResumeServiceResult:
    """Validate and optionally spawn a backend-native resume command."""
    plan, capability = validate_resume_target(target)
    action: Literal["inspect", "spawned"] = "inspect"
    command: str | None = None
    spawned_display_name: str | None = None
    tmux_session: str | None = None
    pane_id: str | None = None

    if spawn_service is not None:
        base_command = spawn_service.resolve_command(target.backend, profile)
        command = spawn_service.resume_command(
            base_command,
            backend=target.backend,
            resume_plan=plan,
        )

    if not dry_run:
        if spawn_service is None:
            raise ResumeUnavailableError(
                error="spawn_service_unavailable",
                message="Backend resume requires the daemon spawn service.",
                capability="unavailable",
            )
        spawn_result = spawn_service.spawn(
            path=target.project_path,
            backend=target.backend,
            profile=profile,
            circle=circle,
            message=message,
            role=role,
            peer_id=peer_id,
            resume_plan=plan,
        )
        action = "spawned"
        command = command or spawn_service.resume_command(
            spawn_service.resolve_command(target.backend, profile),
            backend=target.backend,
            resume_plan=plan,
        )
        spawned_display_name = spawn_result.display_name
        tmux_session = spawn_result.tmux_session
        pane_id = spawn_result.pane_id

    return ResumeServiceResult(
        status="resume_available",
        capability="supported",
        message=(
            "Backend resume spawned for this runtime session."
            if action == "spawned"
            else "Backend resume is available for this runtime session."
        ),
        target=target,
        resume_plan=plan,
        resume_capability=capability,
        action=action,
        command=command,
        spawned_display_name=spawned_display_name,
        tmux_session=tmux_session,
        pane_id=pane_id,
    )
