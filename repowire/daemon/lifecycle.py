"""Request models for lifecycle event endpoints."""

from __future__ import annotations

from pydantic import BaseModel, Field


class PaneDiedRequest(BaseModel):
    pane_id: str = Field(..., min_length=1, max_length=64)


class SessionClosedRequest(BaseModel):
    session_name: str = Field(..., min_length=1, max_length=64)


class SessionRenamedRequest(BaseModel):
    new_name: str = Field(..., min_length=1, max_length=64)
    pane_ids: list[str]


class WindowRenamedRequest(BaseModel):
    session_name: str = Field(..., min_length=1, max_length=64)
    new_name: str = Field(..., min_length=1, max_length=64)
    pane_ids: list[str]


class ClientDetachedRequest(BaseModel):
    session_name: str = Field(..., min_length=1, max_length=64)
