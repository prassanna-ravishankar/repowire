"""Shared models and utilities for route handlers."""

from __future__ import annotations

import re

from pydantic import BaseModel

VALID_NAME_RE = re.compile(r"^[a-zA-Z0-9._-]+$")
MAX_NAME_LEN = 64


def is_valid_identifier(value: str) -> bool:
    """Check if a string is a valid peer/circle identifier."""
    return bool(VALID_NAME_RE.match(value)) and len(value) <= MAX_NAME_LEN


class OkResponse(BaseModel):
    """Simple OK response."""

    ok: bool = True
