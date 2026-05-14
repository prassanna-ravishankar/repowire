"""Tests for /reviews HTTP routes."""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from typing import cast
from unittest.mock import AsyncMock, patch
from urllib.parse import quote

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.gh_pr import PRInfo, _clear_cache_for_tests
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.review_queue_store import ReviewQueueStore
from repowire.daemon.routes import reviews


def _make_app(tmp_path: Path) -> tuple[FastAPI, ReviewQueueStore]:
    cfg = Config()
    store = ReviewQueueStore(tmp_path / "rq.json")
    state = SimpleNamespace(
        config=cfg,
        review_queue_store=store,
        relay_mode=False,
    )
    init_deps(cfg, cast(PeerRegistry, None), state)

    app = FastAPI()
    app.include_router(reviews.router)
    return app, store


@pytest.fixture
async def env(tmp_path):
    _clear_cache_for_tests()
    app, store = _make_app(tmp_path)
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as c:
        yield c, store
    cleanup_deps()
    _clear_cache_for_tests()


async def test_post_reviews_with_explicit_sha(env):
    client, store = env
    r = await client.post(
        "/reviews",
        json={
            "reviewer": "alice",
            "pr_url": "https://github.com/o/r/pull/1",
            "last_reviewed_sha": "abc123",
        },
    )
    assert r.status_code == 200, r.text
    entries = store.list_for("alice")
    assert len(entries) == 1
    assert entries[0].last_reviewed_sha == "abc123"


async def test_post_reviews_fetches_sha_when_omitted(env):
    client, store = env
    with patch.object(
        reviews, "fetch_pr_info",
        AsyncMock(return_value=PRInfo(head_sha="zzz999", state="open")),
    ):
        r = await client.post(
            "/reviews",
            json={"reviewer": "alice", "pr_url": "https://github.com/o/r/pull/1"},
        )
    assert r.status_code == 200, r.text
    assert store.list_for("alice")[0].last_reviewed_sha == "zzz999"


async def test_post_reviews_handles_gh_unreachable(env):
    client, store = env
    with patch.object(
        reviews, "fetch_pr_info",
        AsyncMock(return_value=PRInfo(head_sha=None, state="unknown")),
    ):
        r = await client.post(
            "/reviews",
            json={"reviewer": "alice", "pr_url": "https://github.com/o/r/pull/1"},
        )
    assert r.status_code == 200
    assert store.list_for("alice")[0].last_reviewed_sha is None


async def test_get_reviews_enriches_with_state(env):
    client, store = env
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    store.upsert("alice", "https://github.com/o/r/pull/2", "def")

    async def fake_fetch(pr_url):
        if pr_url.endswith("/1"):
            return PRInfo(head_sha="abc", state="open")  # matches → none-needed
        return PRInfo(head_sha="newer", state="open")  # mismatch → re-review

    with patch.object(reviews, "fetch_pr_info", AsyncMock(side_effect=fake_fetch)):
        r = await client.get("/reviews", params={"reviewer": "alice"})
    assert r.status_code == 200, r.text
    items = {row["pr_url"]: row for row in r.json()["reviews"]}
    assert items["https://github.com/o/r/pull/1"]["my_action"] == "none-needed"
    assert items["https://github.com/o/r/pull/2"]["my_action"] == "re-review-suggested"


async def test_get_reviews_merged_and_closed_actions(env):
    client, store = env
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    store.upsert("alice", "https://github.com/o/r/pull/2", "def")
    store.upsert("alice", "https://github.com/o/r/pull/3", "ghi")

    async def fake_fetch(pr_url):
        return {
            "https://github.com/o/r/pull/1": PRInfo(head_sha="abc", state="merged"),
            "https://github.com/o/r/pull/2": PRInfo(head_sha="def", state="closed"),
            "https://github.com/o/r/pull/3": PRInfo(head_sha=None, state="unknown"),
        }[pr_url]

    with patch.object(reviews, "fetch_pr_info", AsyncMock(side_effect=fake_fetch)):
        r = await client.get("/reviews", params={"reviewer": "alice"})
    items = {row["pr_url"]: row for row in r.json()["reviews"]}
    assert items["https://github.com/o/r/pull/1"]["my_action"] == "merged-since-review"
    assert items["https://github.com/o/r/pull/2"]["my_action"] == "closed-since-review"
    assert items["https://github.com/o/r/pull/3"]["my_action"] == "unknown"


async def test_get_reviews_empty_when_unknown_reviewer(env):
    client, _ = env
    r = await client.get("/reviews", params={"reviewer": "ghost"})
    assert r.status_code == 200
    assert r.json() == {"reviews": []}


async def test_delete_review(env):
    client, store = env
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    encoded = quote("https://github.com/o/r/pull/1", safe="")
    r = await client.delete(f"/reviews/{encoded}", params={"reviewer": "alice"})
    assert r.status_code == 200, r.text
    assert store.list_for("alice") == []


async def test_delete_missing_returns_404(env):
    client, _ = env
    encoded = quote("https://github.com/o/r/pull/999", safe="")
    r = await client.delete(f"/reviews/{encoded}", params={"reviewer": "alice"})
    assert r.status_code == 404


async def test_merged_pr_remains_tracked_until_deleted(env):
    """The merged-since-review surface IS the value — don't auto-prune."""
    client, store = env
    store.upsert("alice", "https://github.com/o/r/pull/1", "abc")
    with patch.object(
        reviews, "fetch_pr_info",
        AsyncMock(return_value=PRInfo(head_sha="abc", state="merged")),
    ):
        r = await client.get("/reviews", params={"reviewer": "alice"})
    assert r.json()["reviews"][0]["my_action"] == "merged-since-review"
    # And it's still there
    assert len(store.list_for("alice")) == 1
