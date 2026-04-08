"""Lifecycle event endpoints — provider-agnostic (tmux, containers, etc.)."""

from __future__ import annotations

from fastapi import APIRouter, Depends

from repowire.daemon.auth import require_localhost
from repowire.daemon.deps import get_lifecycle_handler
from repowire.daemon.lifecycle import (
    ClientDetachedRequest,
    PaneDiedRequest,
    SessionClosedRequest,
    SessionRenamedRequest,
    WindowRenamedRequest,
)
from repowire.daemon.routes._shared import OkResponse

router = APIRouter(tags=["lifecycle"])


@router.post("/hooks/lifecycle/pane-died")
async def hook_pane_died(
    request: PaneDiedRequest,
    _: None = Depends(require_localhost),
) -> OkResponse:
    handler = get_lifecycle_handler()
    await handler.handle_pane_died(request.pane_id)
    return OkResponse()


@router.post("/hooks/lifecycle/session-closed")
async def hook_session_closed(
    request: SessionClosedRequest,
    _: None = Depends(require_localhost),
) -> OkResponse:
    handler = get_lifecycle_handler()
    await handler.handle_session_closed(request.session_name)
    return OkResponse()


@router.post("/hooks/lifecycle/session-renamed")
async def hook_session_renamed(
    request: SessionRenamedRequest,
    _: None = Depends(require_localhost),
) -> OkResponse:
    handler = get_lifecycle_handler()
    await handler.handle_session_renamed(request.new_name, request.pane_ids)
    return OkResponse()


@router.post("/hooks/lifecycle/window-renamed")
async def hook_window_renamed(
    request: WindowRenamedRequest,
    _: None = Depends(require_localhost),
) -> OkResponse:
    handler = get_lifecycle_handler()
    await handler.handle_window_renamed(
        request.session_name, request.new_name, request.pane_ids,
    )
    return OkResponse()


@router.post("/hooks/lifecycle/client-detached")
async def hook_client_detached(
    request: ClientDetachedRequest,
    _: None = Depends(require_localhost),
) -> OkResponse:
    handler = get_lifecycle_handler()
    await handler.handle_client_detached(request.session_name)
    return OkResponse()
