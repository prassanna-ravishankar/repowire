"""Session-targeted control resolution over durable session bindings."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal

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
            "Session bindings require experiments.sqlite_state"
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

    capability = binding.resume_capability or {}
    if _capability_supported(capability):
        return (
            "resume_available",
            "supported",
            "Backend resume metadata is present; callers can use this capability record.",
        )

    return (
        "unsupported",
        "unsupported",
        "No compatible backend resume capability is recorded for this session.",
    )


def _capability_supported(capability: dict[str, Any]) -> bool:
    if capability.get("supported") is True or capability.get("can_resume") is True:
        return True
    status = capability.get("status")
    return isinstance(status, str) and status in {"supported", "available", "resume_available"}
