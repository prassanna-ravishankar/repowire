"""Route tests for GET /peers/{name}/transcript."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.session.history import _encode_cwd


def _make_app(tmp_path: Path):
    cfg = Config()
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state)
    app = FastAPI()
    app.include_router(peers.router)
    return app, registry


def _write_session(projects_root: Path, peer_path: str, entries: list[dict]) -> None:
    session_dir = projects_root / _encode_cwd(peer_path)
    session_dir.mkdir(parents=True, exist_ok=True)
    out = session_dir / "session.jsonl"
    with open(out, "a") as f:
        for e in entries:
            f.write(json.dumps(e) + "\n")


@pytest.mark.anyio
async def test_unknown_peer_returns_404(tmp_path: Path):
    app, _ = _make_app(tmp_path)
    try:
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
            res = await ac.get("/peers/ghost/transcript")
        assert res.status_code == 404
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_peer_with_no_transcripts_returns_empty(tmp_path: Path):
    app, registry = _make_app(tmp_path)
    try:
        _, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CLAUDE_CODE,
            path="/peer/work",
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        with patch(
            "repowire.session.history._claude_projects_dir",
            return_value=tmp_path / "absent",
        ):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(f"/peers/{name}/transcript")
        assert res.status_code == 200
        assert res.json() == {"turns": [], "next_before": None}
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_returns_paginated_turns(tmp_path: Path):
    app, registry = _make_app(tmp_path)
    projects_root = tmp_path / "projects"
    peer_path = "/peer/work"
    try:
        _, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CLAUDE_CODE,
            path=peer_path,
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        _write_session(projects_root, peer_path, [
            {"type": "user", "timestamp": "2026-01-01T00:00:00Z", "sessionId": "s1",
             "message": {"content": "q1"}},
            {"type": "assistant", "timestamp": "2026-01-01T00:00:01Z", "sessionId": "s1",
             "message": {"content": [{"type": "text", "text": "a1"}]}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z", "sessionId": "s1",
             "message": {"content": "q2"}},
            {"type": "assistant", "timestamp": "2026-01-02T00:00:01Z", "sessionId": "s1",
             "message": {"content": [{"type": "text", "text": "a2"}]}},
        ])

        with patch(
            "repowire.session.history._claude_projects_dir",
            return_value=projects_root,
        ):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(f"/peers/{name}/transcript", params={"limit": 2})
                body = res.json()
                assert res.status_code == 200
                assert [t["text"] for t in body["turns"]] == ["a2", "q2"]
                assert body["next_before"] == "2026-01-02T00:00:00Z"

                res2 = await ac.get(
                    f"/peers/{name}/transcript",
                    params={"limit": 10, "before": body["next_before"]},
                )
                body2 = res2.json()
                assert [t["text"] for t in body2["turns"]] == ["a1", "q1"]
                assert body2["next_before"] is None
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_codex_peer_returns_empty_v1(tmp_path: Path):
    app, registry = _make_app(tmp_path)
    try:
        _, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CODEX,
            path="/peer/work",
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
            res = await ac.get(f"/peers/{name}/transcript")
        assert res.status_code == 200
        assert res.json()["turns"] == []
    finally:
        cleanup_deps()


@pytest.fixture
def anyio_backend():
    return "asyncio"
