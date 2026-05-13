"""Scheduled check-in HTTP routes.

POST   /schedules              Create a one-shot scheduled check-in.
GET    /schedules              List schedules (optionally filtered by from_peer).
DELETE /schedules/{id}         Cancel a pending schedule.
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state
from repowire.daemon.routes._shared import OkResponse
from repowire.daemon.schedule_store import Schedule

logger = logging.getLogger(__name__)
router = APIRouter(tags=["schedules"])


class ScheduleCreateRequest(BaseModel):
    from_peer: str = Field(..., description="Display name of the scheduler")
    to_peer: str = Field(..., description="Display name of the recipient")
    text: str = Field(..., description="Message to deliver")
    fire_at: str = Field(..., description="ISO-8601 datetime (UTC or with offset)")
    kind: str = Field("notify", description='"notify" or "ask"')
    circle: str | None = Field(None)


class ScheduleResponse(BaseModel):
    schedule_id: str
    from_peer: str
    to_peer: str
    text: str
    fire_at: str
    kind: str
    circle: str | None
    created_at: str

    @classmethod
    def from_schedule(cls, s: Schedule) -> ScheduleResponse:
        return cls(
            schedule_id=s.schedule_id,
            from_peer=s.from_peer,
            to_peer=s.to_peer,
            text=s.text,
            fire_at=s.fire_at,
            kind=s.kind,
            circle=s.circle,
            created_at=s.created_at,
        )


class ScheduleListResponse(BaseModel):
    schedules: list[ScheduleResponse]


def _parse_fire_at(raw: str) -> datetime:
    try:
        dt = datetime.fromisoformat(raw)
    except ValueError as e:
        raise HTTPException(
            status_code=400,
            detail=f"fire_at must be ISO-8601: {e}",
        ) from e
    if dt.tzinfo is None:
        # Treat naive datetimes as UTC; explicit is better but tolerate it.
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


@router.post("/schedules", response_model=ScheduleResponse)
async def create_schedule(
    request: ScheduleCreateRequest,
    _: str | None = Depends(require_auth),
) -> ScheduleResponse:
    state = get_app_state()
    store = state.schedule_store
    scheduler = state.scheduler

    fire_at = _parse_fire_at(request.fire_at)
    try:
        sched = store.create(
            from_peer=request.from_peer,
            to_peer=request.to_peer,
            text=request.text,
            fire_at=fire_at,
            kind=request.kind,
            circle=request.circle,
        )
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    store.persist()
    scheduler.notify_changed()
    return ScheduleResponse.from_schedule(sched)


@router.get("/schedules", response_model=ScheduleListResponse)
async def list_schedules(
    from_peer: str | None = None,
    _: str | None = Depends(require_auth),
) -> ScheduleListResponse:
    state = get_app_state()
    store = state.schedule_store
    return ScheduleListResponse(
        schedules=[ScheduleResponse.from_schedule(s) for s in store.list_all(from_peer)],
    )


@router.delete("/schedules/{schedule_id}", response_model=OkResponse)
async def delete_schedule(
    schedule_id: str,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    state = get_app_state()
    store = state.schedule_store
    scheduler = state.scheduler
    removed = store.delete(schedule_id)
    if removed is None:
        raise HTTPException(status_code=404, detail=f"No schedule: {schedule_id}")
    store.persist()
    scheduler.notify_changed()
    return OkResponse()
