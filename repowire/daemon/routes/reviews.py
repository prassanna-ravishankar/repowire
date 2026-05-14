"""Review-queue endpoints: track PRs awaiting an agent's re-review.

The store records (reviewer, pr_url, last_reviewed_sha). At GET time we
enrich each entry with the PR's current head SHA + state via `gh api`,
cached for 60s. Entries are never auto-pruned — the merged-since-review
surface is the value.
"""

from __future__ import annotations

import logging
from urllib.parse import unquote

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from repowire.daemon.auth import require_auth
from repowire.daemon.deps import get_app_state
from repowire.daemon.gh_pr import fetch_pr_info
from repowire.daemon.review_queue_store import ReviewQueueStore
from repowire.daemon.routes._shared import OkResponse

logger = logging.getLogger(__name__)
router = APIRouter(tags=["reviews"])


class MarkReviewedRequest(BaseModel):
    reviewer: str = Field(..., description="Display name of the reviewer")
    pr_url: str = Field(..., description="GitHub PR URL")
    last_reviewed_sha: str | None = Field(
        None,
        description=(
            "SHA the reviewer just reviewed. If omitted, daemon fetches the "
            "current head SHA via `gh api` (best-effort)."
        ),
    )


class ReviewItem(BaseModel):
    pr_url: str
    last_reviewed_sha: str | None
    recorded_at: str
    current_head_sha: str | None
    state: str  # open | merged | closed | unknown
    # one of: none-needed, re-review-suggested, merged-since-review,
    # closed-since-review, unknown
    my_action: str


class ReviewListResponse(BaseModel):
    reviews: list[ReviewItem]


def _get_store() -> ReviewQueueStore:
    state = get_app_state()
    store = getattr(state, "review_queue_store", None)
    if store is None:
        raise RuntimeError("review_queue_store not initialized")
    return store


def _derive_action(state: str, last_reviewed_sha: str | None, current_head_sha: str | None) -> str:
    if state == "open":
        if last_reviewed_sha and current_head_sha and last_reviewed_sha == current_head_sha:
            return "none-needed"
        return "re-review-suggested"
    if state == "merged":
        return "merged-since-review"
    if state == "closed":
        return "closed-since-review"
    return "unknown"


@router.post("/reviews", response_model=OkResponse)
async def mark_reviewed(
    request: MarkReviewedRequest,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Record that `reviewer` has reviewed `pr_url` at `last_reviewed_sha`.

    If `last_reviewed_sha` is omitted, the daemon best-effort fetches the
    current head SHA via `gh api`. If that fails too, the entry is recorded
    with a null sha — every future read will surface as `re-review-suggested`
    until the SHA can be filled in.
    """
    store = _get_store()
    sha = request.last_reviewed_sha
    if not sha:
        info = await fetch_pr_info(request.pr_url)
        sha = info.head_sha
    store.upsert(request.reviewer, request.pr_url, sha)
    return OkResponse()


@router.get("/reviews", response_model=ReviewListResponse)
async def list_reviews(
    reviewer: str,
    _: str | None = Depends(require_auth),
) -> ReviewListResponse:
    """List tracked PRs for `reviewer`, enriched with current state."""
    store = _get_store()
    entries = store.list_for(reviewer)
    items: list[ReviewItem] = []
    for entry in entries:
        info = await fetch_pr_info(entry.pr_url)
        action = _derive_action(info.state, entry.last_reviewed_sha, info.head_sha)
        items.append(
            ReviewItem(
                pr_url=entry.pr_url,
                last_reviewed_sha=entry.last_reviewed_sha,
                recorded_at=entry.recorded_at,
                current_head_sha=info.head_sha,
                state=info.state,
                my_action=action,
            )
        )
    return ReviewListResponse(reviews=items)


@router.delete("/reviews/{pr_url_encoded:path}", response_model=OkResponse)
async def delete_review(
    pr_url_encoded: str,
    reviewer: str,
    _: str | None = Depends(require_auth),
) -> OkResponse:
    """Remove a tracked PR for `reviewer`. 404 if the entry wasn't tracked."""
    store = _get_store()
    pr_url = unquote(pr_url_encoded)
    if not store.delete(reviewer, pr_url):
        raise HTTPException(status_code=404, detail="No tracked review for that PR")
    return OkResponse()
