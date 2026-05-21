"""Session-targeted control endpoints."""

from __future__ import annotations

from typing import Literal

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state
from repowire.daemon.peer_delivery import peer_delivery_from_state
from repowire.daemon.session_controls import (
    SessionBindingStoreUnavailableError,
    resolve_session_binding,
    resume_capability_for,
)
from repowire.daemon.websocket_transport import TransportError
from repowire.protocol.messages import AttachmentRef

router = APIRouter(tags=["sessions"])


class SessionNotifyRequest(BaseModel):
    """Fire-and-forget control message targeted by Repowire session ID."""

    from_peer: str = Field(default="dashboard", description="Sending peer or surface")
    text: str = Field(..., description="Notification/control text")
    attachments: list[AttachmentRef] = Field(default_factory=list)
    bypass_circle: bool = Field(
        default=True,
        description="Dashboard/API callers default to bypassing circle checks",
    )


class SessionNotifyResponse(BaseModel):
    """Response for a session-targeted notify control."""

    ok: bool = True
    repowire_session_id: str
    session_status: str
    capability: Literal["active_executor"]
    executor_peer_id: str
    executor_peer_name: str
    delivery_state: Literal["delivered", "queued"]
    delivered: bool
    queued: bool
    reason: str
    hook_delivery: dict | None = None


class SessionResumeRequest(BaseModel):
    """Inspect or request the compatible resume path for a Repowire session."""

    from_peer: str = Field(default="dashboard", description="Caller peer or surface")
    dry_run: bool = Field(
        default=True,
        description="Reserved for future backend resume execution; currently always inspect-only",
    )


class SessionResumeResponse(BaseModel):
    """Current resume capability for a session binding."""

    ok: bool = True
    repowire_session_id: str
    session_status: str
    status: Literal[
        "active_executor",
        "resume_available",
        "unsupported",
        "binding_unavailable",
    ]
    capability: Literal["active_executor", "supported", "unsupported", "unavailable"]
    message: str
    backend: str
    runtime_session_id: str | None = None
    runtime_source_uri: str | None = None
    executor_peer_id: str | None = None
    executor_peer_name: str | None = None
    resume_capability: dict = Field(default_factory=dict)


@router.post(
    "/sessions/{repowire_session_id}/controls/notify",
    response_model=SessionNotifyResponse,
)
async def notify_session(
    repowire_session_id: str,
    request: SessionNotifyRequest,
    _auth: str | None = Depends(require_auth),
) -> SessionNotifyResponse:
    """Send a notify/control message to the active executor for a session."""
    state = get_app_state()
    resolution = await _resolve_or_http(state, repowire_session_id)
    if not resolution.has_active_executor or resolution.executor is None:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "session_executor_unavailable",
                "repowire_session_id": repowire_session_id,
                "session_status": resolution.binding.status,
                "executor_peer_id": resolution.executor_peer_id,
                "capability": "unsupported",
            },
        )

    try:
        delivery = await peer_delivery_from_state(
            config=state.config,
            registry=state.peer_registry,
            state=state,
        ).notify_result(
            from_peer=request.from_peer,
            to_peer=resolution.executor.peer_id,
            text=request.text,
            bypass_circle=request.bypass_circle,
            attachments=request.attachments,
        )
    except TransportError as exc:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "error": "transport_unavailable",
                "repowire_session_id": repowire_session_id,
                "executor_peer_id": resolution.executor.peer_id,
                "message": str(exc),
            },
        ) from exc
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "session_delivery_rejected",
                "repowire_session_id": repowire_session_id,
                "message": str(exc),
            },
        ) from exc

    return SessionNotifyResponse(
        repowire_session_id=repowire_session_id,
        session_status=resolution.binding.status,
        capability="active_executor",
        executor_peer_id=resolution.executor.peer_id,
        executor_peer_name=resolution.executor.display_name,
        delivery_state=delivery.delivery_state,
        delivered=delivery.delivered,
        queued=delivery.queued,
        reason=delivery.reason,
        hook_delivery=delivery.hook_delivery,
    )


@router.post(
    "/sessions/{repowire_session_id}/controls/resume",
    response_model=SessionResumeResponse,
)
async def resume_session(
    repowire_session_id: str,
    request: SessionResumeRequest,
    _auth: str | None = Depends(require_auth),
) -> SessionResumeResponse:
    """Return the compatible resume capability for a session binding.

    This v0.13 slice is API-first: active sessions resolve to their current
    executor, and detached sessions expose recorded backend capability metadata
    or a clear unsupported status. Backend-specific resume execution can be
    layered onto this contract without changing peer-targeted ask/notify APIs.
    """
    state = get_app_state()
    resolution = await _resolve_or_http(state, repowire_session_id)
    resume_status, capability, message = resume_capability_for(resolution)
    executor = resolution.executor if resolution.has_active_executor else None
    binding = resolution.binding
    return SessionResumeResponse(
        repowire_session_id=repowire_session_id,
        session_status=binding.status,
        status=resume_status,
        capability=capability,
        message=message,
        backend=binding.backend,
        runtime_session_id=binding.runtime_session_id,
        runtime_source_uri=binding.runtime_source_uri,
        executor_peer_id=executor.peer_id if executor else resolution.executor_peer_id,
        executor_peer_name=executor.display_name if executor else None,
        resume_capability=binding.resume_capability,
    )


async def _resolve_or_http(state: object, repowire_session_id: str):
    try:
        resolution = await resolve_session_binding(
            state=state,
            repowire_session_id=repowire_session_id,
        )
    except SessionBindingStoreUnavailableError as exc:
        raise HTTPException(
            status_code=422,
            detail={
                "error": "session_bindings_unavailable",
                "message": str(exc),
            },
        ) from exc
    if resolution is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={
                "error": "session_not_found",
                "repowire_session_id": repowire_session_id,
            },
        )
    return resolution
