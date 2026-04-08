"""Request models for lifecycle event endpoints."""

from __future__ import annotations

from pydantic import BaseModel


class PaneDiedRequest(BaseModel):
    pane_id: str


class SessionClosedRequest(BaseModel):
    session_name: str


class SessionRenamedRequest(BaseModel):
    old_name: str
    new_name: str


class WindowRenamedRequest(BaseModel):
    session_name: str
    old_name: str
    new_name: str


class ClientDetachedRequest(BaseModel):
    session_name: str
