"""Session-targeted control resolution over durable session bindings."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from repowire.agent_backends import agent_backend_for, can_resume_backend
from repowire.agent_types import AgentType
from repowire.daemon.state.session_bindings import SessionBinding, SQLiteSessionBindingStore
from repowire.protocol.peers import Peer, PeerStatus

Capability = Literal["active_executor", "supported", "unsupported", "unavailable"]
ResumeStatus = Literal[
    "active_executor",
    "resume_available",
    "unsupported",
    "binding_unavailable",
]


class SessionBindingStoreUnavailableError(RuntimeError):
    """Raised when session bindings are not configured for this daemon."""


@dataclass(frozen=True)
class SessionResolution:
    """Resolved binding plus the current live executor, when attached."""

    binding: SessionBinding
    executor: Peer | None

    @property
    def executor_peer_id(self) -> str | None:
        return self.binding.current_executor_peer_id or self.binding.peer_id

    @property
    def has_active_executor(self) -> bool:
        return self.executor is not None and self.executor.status != PeerStatus.OFFLINE


def get_session_binding_store(state: object) -> SQLiteSessionBindingStore:
    """Return the configured session binding store or raise a typed error."""
    store = getattr(state, "session_binding_store", None)
    if store is None:
        raise SessionBindingStoreUnavailableError(
            "Session bindings require the daemon SQLite state store"
        )
    return store


async def resolve_session_binding(
    *,
    state: object,
    repowire_session_id: str,
) -> SessionResolution | None:
    """Resolve a Repowire session binding to its current executor peer."""
    store = get_session_binding_store(state)
    binding = store.get(repowire_session_id)
    if binding is None:
        return None

    executor_id = binding.current_executor_peer_id or binding.peer_id
    executor: Peer | None = None
    if executor_id:
        registry = getattr(state, "peer_registry")
        try:
            executor = await registry.get_peer(executor_id)
        except ValueError:
            executor = None
    return SessionResolution(binding=binding, executor=executor)


def resume_capability_for(resolution: SessionResolution) -> tuple[ResumeStatus, Capability, str]:
    """Return the compatible resume status for a resolved session binding."""
    binding = resolution.binding
    if resolution.has_active_executor:
        return (
            "active_executor",
            "active_executor",
            "Session already has an active executor; send controls to that peer.",
        )

    if binding.status in {"archived", "lost", "superseded"}:
        return (
            "unsupported",
            "unavailable",
            f"Session binding status is {binding.status}; resume is not available.",
        )

    try:
        backend = AgentType(binding.backend)
    except ValueError:
        backend = None

    if not binding.runtime_session_id:
        return (
            "unsupported",
            "unavailable",
            "This is a legacy or partial session binding without a runtime "
            "session id; resume is not available.",
        )

    if backend is not None and not agent_backend_for(backend).supports_local_spawn:
        return (
            "unsupported",
            "unsupported",
            f"{binding.backend} is a service identity, not a resumable local agent session.",
        )

    can_resume = (
        can_resume_backend(
            backend,
            runtime_session_id=binding.runtime_session_id,
        )
        if backend is not None
        else False
    )
    if can_resume:
        return (
            "resume_available",
            "supported",
            "Backend resume is available for this runtime session.",
        )

    return (
        "unsupported",
        "unsupported",
        "No compatible backend resume capability is recorded for this session.",
    )
