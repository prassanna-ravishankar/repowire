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
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.session.history import _encode_cwd


def _make_app(tmp_path: Path, *, with_bindings: bool = False):
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
    if with_bindings:
        db = StateDatabase(tmp_path / "state.db")
        app_state.state_db = db
        app_state.session_binding_store = SQLiteSessionBindingStore(db)
    init_deps(cfg, registry, app_state)
    app = FastAPI()
    app.include_router(peers.router)
    return app, registry, getattr(app_state, "session_binding_store", None)


def _write_session(projects_root: Path, peer_path: str, entries: list[dict]) -> None:
    session_dir = projects_root / _encode_cwd(peer_path)
    session_dir.mkdir(parents=True, exist_ok=True)
    out = session_dir / "session.jsonl"
    with open(out, "a") as f:
        for e in entries:
            f.write(json.dumps(e) + "\n")


def _write_codex_session(sessions_root: Path, entries: list[dict]) -> Path:
    session_dir = sessions_root / "2026" / "05" / "21"
    session_dir.mkdir(parents=True, exist_ok=True)
    out = session_dir / "rollout-2026-05-21T08-00-00-codex-session.jsonl"
    with open(out, "w") as f:
        for entry in entries:
            f.write(json.dumps(entry) + "\n")
    return out


@pytest.mark.anyio
async def test_unknown_peer_returns_404(tmp_path: Path):
    app, _, _ = _make_app(tmp_path)
    try:
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
            res = await ac.get("/peers/ghost/transcript")
        assert res.status_code == 404
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_peer_with_no_transcripts_returns_empty(tmp_path: Path):
    app, registry, _ = _make_app(tmp_path)
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
        assert res.json() == {
            "turns": [],
            "next_before": None,
            "history_status": "unavailable",
            "history_backend": "claude-code",
            "history_message": "No Claude Code transcript history found for this peer path.",
            "history_source": "peer_path",
            "repowire_session_id": None,
            "binding_status": None,
            "runtime_session_id": None,
        }
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_returns_paginated_turns(tmp_path: Path):
    app, registry, _ = _make_app(tmp_path)
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
                assert body["turns"][0]["turn_id"].startswith("history:s1:")
                assert body["next_before"] is not None

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
async def test_session_id_filter_applies_before_pagination_and_returns_turn_id(tmp_path: Path):
    app, registry, _ = _make_app(tmp_path)
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
            {"type": "user", "timestamp": "2026-01-01T00:00:00Z",
             "sessionId": "s1", "message": {"content": "s1 old prompt"}},
            {"type": "assistant", "uuid": "s1-old-id", "timestamp": "2026-01-01T00:00:00Z",
             "sessionId": "s1", "message": {"content": [{"type": "text", "text": "s1 old"}]}},
            {"type": "user", "timestamp": "2026-01-03T00:00:00Z",
             "sessionId": "s2", "message": {"content": "s2 prompt"}},
            {"type": "assistant", "uuid": "s2-new-id", "timestamp": "2026-01-03T00:00:00Z",
             "sessionId": "s2", "message": {"content": [{"type": "text", "text": "s2 new"}]}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z",
             "sessionId": "s1", "message": {"content": "s1 new prompt"}},
            {"type": "assistant", "uuid": "s1-new-id", "timestamp": "2026-01-02T00:00:00Z",
             "sessionId": "s1", "message": {"content": [{"type": "text", "text": "s1 new"}]}},
        ])

        with patch(
            "repowire.session.history._claude_projects_dir",
            return_value=projects_root,
        ):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(
                    f"/peers/{name}/transcript",
                    params={"session_id": "s1", "limit": 2},
                )
                body = res.json()
                assert [t["text"] for t in body["turns"]] == ["s1 new", "s1 new prompt"]
                assert body["turns"][0]["turn_id"] == "s1-new-id"
                assert body["next_before"] is not None

                res2 = await ac.get(
                    f"/peers/{name}/transcript",
                    params={
                        "session_id": "s1",
                        "limit": 2,
                        "before": body["next_before"],
                    },
                )
                body2 = res2.json()
                assert [t["text"] for t in body2["turns"]] == ["s1 old", "s1 old prompt"]
                assert body2["turns"][0]["turn_id"] == "s1-old-id"
                assert body2["next_before"] is None
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_codex_peer_returns_empty_v1(tmp_path: Path):
    app, registry, _ = _make_app(tmp_path)
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


@pytest.mark.anyio
async def test_same_timestamp_boundary_does_not_drop_turns(tmp_path: Path):
    """Regression: when the page-boundary timestamp is shared by multiple
    turns, the cursor must include a tiebreaker so they survive pagination."""
    app, registry, _ = _make_app(tmp_path)
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
        # Five turns; three share the same timestamp at the page boundary.
        _write_session(projects_root, peer_path, [
            {"type": "user", "timestamp": "2026-01-01T00:00:00Z", "sessionId": "s1",
             "message": {"content": "oldest"}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z", "sessionId": "s1",
             "message": {"content": "mid_a"}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z", "sessionId": "s1",
             "message": {"content": "mid_b"}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z", "sessionId": "s1",
             "message": {"content": "mid_c"}},
            {"type": "user", "timestamp": "2026-01-03T00:00:00Z", "sessionId": "s1",
             "message": {"content": "newest"}},
        ])

        with patch(
            "repowire.session.history._claude_projects_dir",
            return_value=projects_root,
        ):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                # Page 1: limit=2 lands the boundary mid-cluster.
                res = await ac.get(f"/peers/{name}/transcript", params={"limit": 2})
                body = res.json()
                texts: list[str] = [t["text"] for t in body["turns"]]
                cursor = body["next_before"]
                assert cursor is not None
                while cursor is not None:
                    res = await ac.get(
                        f"/peers/{name}/transcript",
                        params={"limit": 2, "before": cursor},
                    )
                    body = res.json()
                    texts.extend(t["text"] for t in body["turns"])
                    cursor = body["next_before"]
                # All five turns must be present; no drop at the boundary.
                assert sorted(texts) == sorted(
                    ["newest", "mid_a", "mid_b", "mid_c", "oldest"]
                )
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_empty_timestamp_turns_reachable_via_cursor(tmp_path: Path):
    """Regression: turns with empty timestamps used to be included on the
    first page but dropped on paginated requests."""
    app, registry, _ = _make_app(tmp_path)
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
            {"type": "user", "sessionId": "s1",
             "message": {"content": "no_ts_a"}},
            {"type": "user", "sessionId": "s1",
             "message": {"content": "no_ts_b"}},
            {"type": "user", "timestamp": "2026-01-01T00:00:00Z", "sessionId": "s1",
             "message": {"content": "older"}},
            {"type": "user", "timestamp": "2026-01-02T00:00:00Z", "sessionId": "s1",
             "message": {"content": "newer"}},
        ])

        with patch(
            "repowire.session.history._claude_projects_dir",
            return_value=projects_root,
        ):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                texts: list[str] = []
                cursor: str | None = None
                for _ in range(10):
                    params: dict[str, object] = {"limit": 1}
                    if cursor is not None:
                        params["before"] = cursor
                    res = await ac.get(f"/peers/{name}/transcript", params=params)
                    body = res.json()
                    texts.extend(t["text"] for t in body["turns"])
                    cursor = body["next_before"]
                    if cursor is None:
                        break
                assert "no_ts_a" in texts
                assert "no_ts_b" in texts
                assert sorted(texts) == sorted(["newer", "older", "no_ts_a", "no_ts_b"])
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_claude_transcript_resolves_from_session_binding(tmp_path: Path):
    app, registry, binding_store = _make_app(tmp_path, with_bindings=True)
    assert binding_store is not None
    projects_root = tmp_path / "projects"
    peer_path = "/peer/work"
    try:
        peer_id, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CLAUDE_CODE,
            path=peer_path,
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        _write_session(projects_root, peer_path, [
            {"type": "user", "timestamp": "2026-01-01T00:00:00Z", "sessionId": "bound-s1",
             "message": {"content": "bound prompt"}},
            {"type": "assistant", "uuid": "bound-turn", "timestamp": "2026-01-01T00:00:01Z",
             "sessionId": "bound-s1",
             "message": {"content": [{"type": "text", "text": "bound answer"}]}},
        ])
        binding = binding_store.upsert_observation(
            peer_id=peer_id,
            backend=AgentType.CLAUDE_CODE,
            project_path=peer_path,
            runtime_session_id="bound-s1",
            runtime_source_uri=f"claude-jsonl:{_encode_cwd(peer_path)}/session.jsonl",
            provenance={"source_kind": "runtime_transcript"},
        )

        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(
                    f"/peers/{name}/transcript",
                    params={"session_id": "bound-s1"},
                )

        assert res.status_code == 200
        body = res.json()
        assert [turn["text"] for turn in body["turns"]] == ["bound answer", "bound prompt"]
        assert body["history_source"] == "session_binding"
        assert body["repowire_session_id"] == binding.repowire_session_id
        assert body["binding_status"] == "active"
        assert body["runtime_session_id"] == "bound-s1"
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_codex_transcript_resolves_from_session_binding(tmp_path: Path):
    app, registry, binding_store = _make_app(tmp_path, with_bindings=True)
    assert binding_store is not None
    sessions_root = tmp_path / "codex-sessions"
    peer_path = "/peer/work"
    try:
        peer_id, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CODEX,
            path=peer_path,
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        _write_codex_session(sessions_root, [
            {"type": "session_meta", "payload": {"id": "codex-bound-s1", "cwd": peer_path}},
            {"type": "response_item", "timestamp": "2026-05-21T08:00:01Z",
             "payload": {"type": "message", "role": "user",
                         "content": [{"type": "input_text", "text": "codex bound prompt"}]}},
            {"type": "turn_context", "payload": {"turn_id": "codex-bound-turn", "cwd": peer_path}},
            {"type": "response_item", "timestamp": "2026-05-21T08:00:02Z",
             "payload": {"type": "message", "role": "assistant",
                         "content": [{"type": "output_text", "text": "codex bound answer"}]}},
        ])
        binding = binding_store.upsert_observation(
            peer_id=peer_id,
            backend=AgentType.CODEX,
            project_path=peer_path,
            runtime_session_id="codex-bound-s1",
            runtime_source_uri="codex-rollout:2026/05/21/rollout-2026-05-21T08-00-00-codex-session.jsonl",
            provenance={"source_kind": "runtime_transcript"},
        )

        with patch("repowire.session.history._codex_sessions_dir", return_value=sessions_root):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(
                    f"/peers/{name}/transcript",
                    params={"session_id": "codex-bound-s1"},
                )

        assert res.status_code == 200
        body = res.json()
        assert [turn["text"] for turn in body["turns"]] == [
            "codex bound answer",
            "codex bound prompt",
        ]
        assert body["history_source"] == "session_binding"
        assert body["repowire_session_id"] == binding.repowire_session_id
        assert body["runtime_session_id"] == "codex-bound-s1"
    finally:
        cleanup_deps()


@pytest.fixture
def anyio_backend():
    return "asyncio"
