"""Attachment upload/download endpoints."""

from __future__ import annotations

import logging
import time
from pathlib import Path
from uuid import uuid4

from fastapi import APIRouter, Depends, HTTPException, UploadFile, status
from fastapi.responses import FileResponse

from repowire.daemon.auth import require_auth

logger = logging.getLogger(__name__)

router = APIRouter(tags=["attachments"])

ATTACHMENTS_DIR = Path.home() / ".repowire" / "attachments"
MAX_FILE_SIZE = 10 * 1024 * 1024  # 10MB
MAX_AGE_HOURS = 24
MAX_DIR_SIZE = 200 * 1024 * 1024  # 200MB total cap; 507 when exceeded


def _ensure_dir() -> Path:
    ATTACHMENTS_DIR.mkdir(parents=True, exist_ok=True)
    return ATTACHMENTS_DIR


def _cleanup_expired() -> None:
    """Remove attachments older than MAX_AGE_HOURS. Best-effort."""
    if not ATTACHMENTS_DIR.exists():
        return
    cutoff = time.time() - (MAX_AGE_HOURS * 3600)
    for f in ATTACHMENTS_DIR.iterdir():
        try:
            if f.stat().st_mtime < cutoff:
                f.unlink(missing_ok=True)
        except OSError:
            pass


def _dir_size() -> int:
    """Total bytes used by the attachments directory. Best-effort."""
    if not ATTACHMENTS_DIR.exists():
        return 0
    total = 0
    for f in ATTACHMENTS_DIR.iterdir():
        try:
            total += f.stat().st_size
        except OSError:
            pass
    return total


@router.post("/attachments")
async def upload_attachment(
    file: UploadFile,
    _: str | None = Depends(require_auth),
) -> dict:
    """Upload a file attachment. Returns metadata for mesh message references."""
    if file.size and file.size > MAX_FILE_SIZE:
        raise HTTPException(
            status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE,
            detail=f"File too large (max {MAX_FILE_SIZE // 1024 // 1024}MB)",
        )

    # Sweep TTL'd files first so a stale-but-expired dir doesn't deny a legitimate upload.
    _cleanup_expired()

    used = _dir_size()
    if used >= MAX_DIR_SIZE:
        raise HTTPException(
            status_code=status.HTTP_507_INSUFFICIENT_STORAGE,
            detail=(
                f"Attachments directory full "
                f"({used // 1024 // 1024}MB used, "
                f"{MAX_DIR_SIZE // 1024 // 1024}MB cap). "
                f"Wait for the {MAX_AGE_HOURS}h TTL to clear older files."
            ),
        )

    ext = Path(file.filename or "file").suffix or ".bin"
    attachment_id = str(uuid4())[:8]
    dest = _ensure_dir() / f"{attachment_id}{ext}"

    size = 0
    with open(dest, "wb") as out:
        while chunk := await file.read(8192):
            size += len(chunk)
            if size > MAX_FILE_SIZE:
                dest.unlink(missing_ok=True)
                raise HTTPException(
                    status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE,
                    detail=f"File too large (max {MAX_FILE_SIZE // 1024 // 1024}MB)",
                )
            if used + size > MAX_DIR_SIZE:
                dest.unlink(missing_ok=True)
                raise HTTPException(
                    status_code=status.HTTP_507_INSUFFICIENT_STORAGE,
                    detail=(
                        f"Attachments directory would exceed cap "
                        f"({MAX_DIR_SIZE // 1024 // 1024}MB). "
                        f"Wait for the {MAX_AGE_HOURS}h TTL to clear older files."
                    ),
                )
            out.write(chunk)

    logger.info("Attachment saved: %s (%d bytes)", dest.name, size)

    return {
        "id": attachment_id,
        "path": str(dest),
        "filename": file.filename or dest.name,
        "size": size,
        "content_type": file.content_type,
    }


@router.get("/attachments/{attachment_id}")
async def get_attachment(
    attachment_id: str,
    _: str | None = Depends(require_auth),
) -> FileResponse:
    """Download an attachment by ID."""
    if not ATTACHMENTS_DIR.exists():
        raise HTTPException(status_code=404, detail="Not found")

    # Find file matching the ID prefix
    for f in ATTACHMENTS_DIR.iterdir():
        if f.stem == attachment_id:
            return FileResponse(f, filename=f.name)

    raise HTTPException(status_code=404, detail="Attachment not found")
