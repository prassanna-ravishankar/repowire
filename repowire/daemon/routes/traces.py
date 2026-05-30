"""Delivery trace inspection endpoint.

Exposes the per-message delivery ledger so an operator can answer
"where did this ask/notify go?" without external tracing infrastructure.
"""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state

router = APIRouter(tags=["traces"])


class TraceStageInfo(BaseModel):
    """One recorded delivery stage."""

    seq: int
    stage: str
    status: str
    peer_id: str | None = None
    from_peer_id: str | None = None
    ts: str
    detail: dict


class TraceResponse(BaseModel):
    """All recorded stages for a delivery trace, in order."""

    trace_id: str
    kind: str | None = None
    stages: list[TraceStageInfo]


@router.get("/traces/{trace_id}", response_model=TraceResponse)
async def get_trace(
    trace_id: str,
    _: str | None = Depends(require_auth),
) -> TraceResponse:
    """Return the ordered delivery stages for a trace_id.

    trace_id is an ask correlation_id or a notify delivery_id.
    """
    state = get_app_state()
    store = getattr(state, "delivery_trace_store", None)
    if store is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Delivery trace ledger is not available",
        )
    rows = store.stages_for(trace_id)
    if not rows:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"No delivery trace for: {trace_id}",
        )
    return TraceResponse(
        trace_id=trace_id,
        kind=rows[0].kind,
        stages=[
            TraceStageInfo(
                seq=r.seq,
                stage=r.stage,
                status=r.status,
                peer_id=r.peer_id,
                from_peer_id=r.from_peer_id,
                ts=r.ts,
                detail=r.detail,
            )
            for r in rows
        ],
    )
