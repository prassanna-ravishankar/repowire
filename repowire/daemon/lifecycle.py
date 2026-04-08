"""Request models for lifecycle event endpoints."""

from __future__ import annotations

from pydantic import BaseModel


class PaneDiedRequest(BaseModel):
    pane_id: str


class SessionClosedRequest(BaseModel):
    session_name: str


class SessionRenamedRequest(BaseModel):
    new_name: str
    pane_ids: list[str]


class WindowRenamedRequest(BaseModel):
    session_name: str
    new_name: str
    pane_ids: list[str]


class ClientDetachedRequest(BaseModel):
    session_name: str
