"""Tracked work lifecycle HTTP routes."""

from __future__ import annotations

import asyncio
from typing import Any

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state
from repowire.daemon.work_store import TrackedWork

router = APIRouter(tags=["work"])


class WorkCreateRequest(BaseModel):
    title: str = ""
    kind: str = "general"
    created_by_peer_id: str | None = None
    owner_peer_id: str | None = None
    assigned_peer_id: str | None = None
    repowire_session_id: str | None = None
    correlation_id: str | None = None
    circle: str | None = None
    source_kind: str | None = Field(None, description="Origin type, e.g. mcp, cli, dashboard")
    source_id: str | None = Field(None, description="Origin-local identifier")
    scope: str | None = Field(None, description="Small routing/display scope label")
    visibility: str = "circle"
    request: dict[str, Any] = Field(default_factory=dict)
    deadline_at: str | None = None
    expires_at: str | None = None
    provenance: dict[str, Any] = Field(default_factory=dict)


class WorkCancelRequest(BaseModel):
    requested_by_peer_id: str | None = None
    reason: str = "cancel_requested"


class WorkUpdateRequest(BaseModel):
    state: str
    state_reason: str | None = None
    phase: str | None = None
    progress: dict[str, Any] | None = None
    progress_note: str | None = None
    result_summary: str | None = None
    result_data: dict[str, Any] | None = None
    error: dict[str, Any] | None = None
    artifacts: list[Any] | None = None
    provenance: dict[str, Any] | None = None


class WorkStatusResponse(BaseModel):
    status: dict[str, Any]

    @classmethod
    def from_work(cls, work: TrackedWork) -> WorkStatusResponse:
        return cls(status=work.status())


class WorkCreateResponse(BaseModel):
    job_id: str
    work_id: str
    status: dict[str, Any]

    @classmethod
    def from_work(cls, work: TrackedWork) -> WorkCreateResponse:
        return cls(job_id=work.work_id, work_id=work.work_id, status=work.status())


class WorkListResponse(BaseModel):
    work: list[dict[str, Any]]


class WorkResultResponse(BaseModel):
    result: dict[str, Any]


def _store():
    state = get_app_state()
    store = getattr(state, "work_store", None)
    if store is None:
        raise RuntimeError("work_store not initialized")
    return store


async def _attempt_protocol_cancel(work: TrackedWork) -> dict[str, Any]:
    """Try a bounded runtime-level cancel for the current architecture slice.

    Tracked work does not yet have a durable execution/session binding for
    every transport. ACP is the only runtime with a daemon-owned live session
    handle today, so this function only targets an existing ACP client for the
    assigned peer and otherwise records an explicit unavailable/skipped result.
    """
    if work.terminal:
        return {
            "attempted": False,
            "status": "skipped",
            "reason": "terminal_work",
        }
    if not work.assigned_peer_id:
        return {
            "attempted": False,
            "status": "unavailable",
            "reason": "no_assigned_peer",
        }
    state = get_app_state()
    manager = getattr(state, "acp_manager", None)
    if manager is None or not hasattr(manager, "cancel_existing"):
        return {
            "attempted": False,
            "status": "unavailable",
            "reason": "no_protocol_cancel_adapter",
            "peer_id": work.assigned_peer_id,
        }
    try:
        result = await asyncio.wait_for(
            manager.cancel_existing(work.assigned_peer_id),
            timeout=2.0,
        )
        return result if isinstance(result, dict) else {
            "attempted": True,
            "status": "sent",
            "reason": "session_cancel_sent",
            "peer_id": work.assigned_peer_id,
        }
    except TimeoutError:
        return {
            "attempted": True,
            "status": "timeout",
            "reason": "protocol_cancel_timeout",
            "peer_id": work.assigned_peer_id,
        }
    except Exception as e:  # noqa: BLE001 - cancel remains audit-visible
        return {
            "attempted": True,
            "status": "failed",
            "reason": "protocol_cancel_failed",
            "peer_id": work.assigned_peer_id,
            "error": str(e),
        }


def _merge_protocol_cancel(
    work: TrackedWork,
    protocol_cancel: dict[str, Any],
) -> dict[str, Any]:
    provenance = dict(work.provenance)
    attempts = list(provenance.get("protocol_cancel_attempts") or [])
    attempts.append(protocol_cancel)
    provenance["protocol_cancel"] = protocol_cancel
    provenance["protocol_cancel_attempts"] = attempts[-10:]
    return provenance


@router.post("/work", response_model=WorkCreateResponse)
@router.post("/jobs", response_model=WorkCreateResponse)
async def create_work(
    request: WorkCreateRequest,
    _: str | None = Depends(require_auth),
) -> WorkCreateResponse:
    store = _store()
    work = store.create(
        title=request.title,
        kind=request.kind,
        created_by_peer_id=request.created_by_peer_id,
        owner_peer_id=request.owner_peer_id,
        assigned_peer_id=request.assigned_peer_id,
        repowire_session_id=request.repowire_session_id,
        correlation_id=request.correlation_id,
        circle=request.circle,
        source_kind=request.source_kind,
        source_id=request.source_id,
        scope=request.scope,
        visibility=request.visibility,
        request=request.request,
        deadline_at=request.deadline_at,
        expires_at=request.expires_at,
        provenance=request.provenance,
    )
    return WorkCreateResponse.from_work(work)


@router.get("/work", response_model=WorkListResponse)
@router.get("/jobs", response_model=WorkListResponse)
async def list_work(
    state: str | None = None,
    owner_peer_id: str | None = None,
    created_by_peer_id: str | None = None,
    repowire_session_id: str | None = None,
    circle: str | None = None,
    _: str | None = Depends(require_auth),
) -> WorkListResponse:
    store = _store()
    try:
        items = store.list_all(
            state=state,
            owner_peer_id=owner_peer_id,
            created_by_peer_id=created_by_peer_id,
            repowire_session_id=repowire_session_id,
            circle=circle,
        )
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    return WorkListResponse(work=[item.status() for item in items])


@router.get("/work/{work_id}/status", response_model=WorkStatusResponse)
@router.get("/jobs/{work_id}", response_model=WorkStatusResponse)
@router.get("/jobs/{work_id}/status", response_model=WorkStatusResponse)
async def get_work_status(
    work_id: str,
    _: str | None = Depends(require_auth),
) -> WorkStatusResponse:
    work = _store().get(work_id)
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    return WorkStatusResponse.from_work(work)


@router.patch("/work/{work_id}", response_model=WorkStatusResponse)
@router.patch("/jobs/{work_id}", response_model=WorkStatusResponse)
async def update_work(
    work_id: str,
    request: WorkUpdateRequest,
    _: str | None = Depends(require_auth),
) -> WorkStatusResponse:
    try:
        work = _store().update_state(
            work_id,
            state=request.state,
            state_reason=request.state_reason,
            phase=request.phase,
            progress=request.progress,
            progress_note=request.progress_note,
            result_summary=request.result_summary,
            result_data=request.result_data,
            error=request.error,
            artifacts=request.artifacts,
            provenance=request.provenance,
        )
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    return WorkStatusResponse.from_work(work)


@router.get("/work/{work_id}/result", response_model=WorkResultResponse)
@router.get("/jobs/{work_id}/result", response_model=WorkResultResponse)
async def get_work_result(
    work_id: str,
    _: str | None = Depends(require_auth),
) -> WorkResultResponse:
    work = _store().get(work_id)
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    return WorkResultResponse(result=work.result())


@router.post("/work/{work_id}/cancel", response_model=WorkStatusResponse)
@router.post("/jobs/{work_id}/cancel", response_model=WorkStatusResponse)
async def cancel_work(
    work_id: str,
    request: WorkCancelRequest,
    _: str | None = Depends(require_auth),
) -> WorkStatusResponse:
    existing = _store().get(work_id)
    work = _store().cancel(
        work_id,
        requested_by_peer_id=request.requested_by_peer_id,
        reason=request.reason,
    )
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    if existing is not None and not existing.terminal and existing.state != "queued":
        protocol_cancel = await _attempt_protocol_cancel(work)
        provenance = _merge_protocol_cancel(work, protocol_cancel)
        updated = _store().update_state(
            work.work_id,
            state=work.state,
            state_reason=work.state_reason,
            phase=work.phase,
            progress=work.progress,
            provenance=provenance,
        )
        if updated is not None:
            work = updated
    return WorkStatusResponse.from_work(work)
