"""Tracked work lifecycle HTTP routes."""

from __future__ import annotations

import asyncio
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.config.models import AgentType
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
    prompt: str | None = Field(None, description="Inline durable job prompt")
    prompt_file: str | None = Field(None, description="Read durable job prompt from file")
    path: str | None = Field(None, description="Target working directory for spawned peer")
    backend: AgentType | None = Field(
        None, description="Backend to spawn when assigned_peer_id is absent"
    )
    profile: str | None = Field(None, description="Optional backend profile")
    due_at: str | None = Field(None, description="Optional scheduled due time")
    result_surface: str | None = Field(None, description="Metadata-only result surface")


class WorkCancelRequest(BaseModel):
    requested_by_peer_id: str | None = None
    reason: str = "cancel_requested"


class WorkRunRequest(BaseModel):
    requested_by_peer_id: str | None = None


class WorkUpdateRequest(BaseModel):
    state: str
    attempt_id: str | None = None
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


def _job_runner():
    state = get_app_state()
    runner = getattr(state, "job_runner", None)
    if runner is None:
        raise RuntimeError("job_runner not initialized")
    return runner


def _read_prompt_file(path: str) -> str:
    from pathlib import Path

    return Path(path).expanduser().read_text(encoding="utf-8")


def _merge_execution_request(
    request: WorkCreateRequest, assigned_peer_id: str | None
) -> dict[str, Any]:
    body = dict(request.request)
    execution = dict(body.get("execution") or {})
    prompt = dict(execution.get("prompt") or {})
    if request.prompt_file:
        prompt.update(
            {
                "body": _read_prompt_file(request.prompt_file),
                "source": "file",
                "source_path": request.prompt_file,
            }
        )
    elif request.prompt is not None:
        prompt.update({"body": request.prompt, "source": "inline"})
    elif not prompt:
        prompt.update({"body": request.title, "source": "title"})
    target = dict(execution.get("target") or {})
    if request.path is not None:
        target["path"] = request.path
    if request.backend is not None:
        target["backend"] = request.backend.value
    if request.profile is not None:
        target["profile"] = request.profile
    if assigned_peer_id is not None:
        target["assigned_peer_id"] = assigned_peer_id
    schedule = dict(execution.get("schedule") or {})
    if request.due_at is not None:
        schedule["due_at"] = request.due_at
    delivery = dict(execution.get("delivery") or {})
    delivery.setdefault("kind", "ask")
    if request.result_surface is not None:
        delivery["result_surface"] = request.result_surface
    execution.update(
        {"prompt": prompt, "target": target, "schedule": schedule, "delivery": delivery}
    )
    body["execution"] = execution
    return body


async def _canonical_assigned_peer(identifier: str | None, circle: str | None) -> str | None:
    if not identifier:
        return None
    if identifier.startswith("repow-"):
        return identifier
    registry = get_app_state().peer_registry
    resolved = await registry.resolve_peer_strict(identifier, circle=circle)
    if isinstance(resolved, list):
        if not resolved:
            raise HTTPException(
                status_code=404, detail={"error": "assigned_peer_not_found", "peer": identifier}
            )
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "error": "ambiguous_assigned_peer",
                "peer": identifier,
                "candidates": [
                    {"peer_id": p.peer_id, "display_name": p.display_name, "circle": p.circle}
                    for p in resolved
                ],
            },
        )
    return resolved.peer_id


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
        return (
            result
            if isinstance(result, dict)
            else {
                "attempted": True,
                "status": "sent",
                "reason": "session_cancel_sent",
                "peer_id": work.assigned_peer_id,
            }
        )
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
    assigned_peer_id = await _canonical_assigned_peer(request.assigned_peer_id, request.circle)
    merged_request = _merge_execution_request(request, assigned_peer_id)
    work = store.create(
        title=request.title,
        kind=request.kind,
        created_by_peer_id=request.created_by_peer_id,
        owner_peer_id=request.owner_peer_id,
        assigned_peer_id=assigned_peer_id,
        repowire_session_id=request.repowire_session_id,
        correlation_id=request.correlation_id,
        circle=request.circle,
        source_kind=request.source_kind,
        source_id=request.source_id,
        scope=request.scope,
        visibility=request.visibility,
        request=merged_request,
        deadline_at=request.deadline_at,
        expires_at=request.expires_at,
        provenance=request.provenance,
    )
    runner = getattr(get_app_state(), "job_runner", None)
    if runner is not None and hasattr(runner, "wake"):
        runner.wake()
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
            attempt_id=request.attempt_id,
        )
    except RuntimeError as e:
        if str(e) == "stale_attempt":
            raise HTTPException(status_code=409, detail="stale attempt_id") from e
        raise
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    return WorkStatusResponse.from_work(work)


@router.post("/work/{work_id}/run", response_model=WorkStatusResponse)
@router.post("/jobs/{work_id}/run", response_model=WorkStatusResponse)
async def run_work(
    work_id: str,
    request: WorkRunRequest | None = None,
    _: str | None = Depends(require_auth),
) -> WorkStatusResponse:
    existing = _store().get(work_id)
    if existing is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    if existing.state not in {"queued", "failed", "unavailable"}:
        raise HTTPException(
            status_code=409, detail=f"Job cannot be run from state {existing.state}"
        )
    work = await _job_runner().run_job(work_id, ignore_due_at=True)
    if work is None:
        work = _store().get(work_id)
    if work is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    return WorkStatusResponse.from_work(work)


@router.post("/work/{work_id}/retry", response_model=WorkStatusResponse)
@router.post("/jobs/{work_id}/retry", response_model=WorkStatusResponse)
async def retry_work(
    work_id: str,
    request: WorkRunRequest | None = None,
    _: str | None = Depends(require_auth),
) -> WorkStatusResponse:
    existing = _store().get(work_id)
    if existing is None:
        raise HTTPException(status_code=404, detail=f"No work: {work_id}")
    if existing.state not in {"failed", "unavailable", "delivered"}:
        raise HTTPException(
            status_code=409, detail=f"Job cannot be retried from state {existing.state}"
        )
    work = await _job_runner().run_job(work_id, ignore_due_at=True, retry=True)
    if work is None:
        work = _store().get(work_id)
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
