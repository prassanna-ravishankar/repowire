"""ACP permission approval endpoints."""

from __future__ import annotations

from typing import Literal

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state

router = APIRouter(tags=["acp"])


class AcpPermissionDecisionRequest(BaseModel):
    """Human decision for a pending ACP permission request."""

    outcome: Literal["allowed", "denied", "cancelled"] = Field(
        ..., description="Permission decision."
    )
    option_id: str | None = Field(
        None,
        description="ACP option_id to select when outcome is allowed.",
    )
    message: str | None = Field(
        None,
        description="Optional human-readable decision note.",
    )


class AcpPermissionDecisionResponse(BaseModel):
    ok: bool = True
    request_id: str
    outcome: Literal["allowed", "denied", "cancelled"]
    option_id: str | None = None


@router.post(
    "/acp/permissions/{request_id}/decision",
    response_model=AcpPermissionDecisionResponse,
)
async def decide_acp_permission(
    request_id: str,
    body: AcpPermissionDecisionRequest,
    _: str | None = Depends(require_auth),
) -> AcpPermissionDecisionResponse:
    """Resolve a pending brokered ACP permission request."""
    state = get_app_state()
    broker = getattr(state, "acp_permission_broker", None)
    if broker is None:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="ACP permission broker not initialized",
        )

    pending = await broker.get_pending(request_id)
    if pending is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"ACP permission request not found: {request_id}",
        )
    if body.outcome == "allowed":
        valid_options = {
            option.get("option_id")
            for option in pending["options"]
            if isinstance(option.get("option_id"), str)
        }
        if body.option_id is not None and body.option_id not in valid_options:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail=f"Unknown ACP permission option_id: {body.option_id}",
            )
        if body.option_id is None and not valid_options:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail="Allowed ACP permission decision requires an option_id",
            )

    decision = await broker.decide(
        request_id,
        outcome=body.outcome,
        option_id=body.option_id,
        message=body.message,
    )
    if decision is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"ACP permission request not found: {request_id}",
        )
    return AcpPermissionDecisionResponse(
        request_id=request_id,
        outcome=decision.outcome,
        option_id=decision.option_id,
    )
