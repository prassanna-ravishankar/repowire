"""Route tests for GET /peers/{name}/timeline."""

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
        for entry in entries:
            f.write(json.dumps(entry) + "\n")


def _write_codex_session(sessions_root: Path, entries: list[dict]) -> None:
    session_dir = sessions_root / "2026" / "05" / "21"
    session_dir.mkdir(parents=True, exist_ok=True)
    out = session_dir / "rollout-2026-05-21T08-00-00-codex-session.jsonl"
    with open(out, "w") as f:
        for entry in entries:
            f.write(json.dumps(entry) + "\n")


@pytest.mark.anyio
async def test_timeline_merges_history_and_realtime_events(tmp_path: Path) -> None:
    app, registry, _ = _make_app(tmp_path)
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
        _write_session(
            projects_root,
            peer_path,
            [
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:00Z",
                    "sessionId": "s1",
                    "message": {"content": "prompt"},
                },
                {
                    "type": "assistant",
                    "uuid": "assistant-turn",
                    "timestamp": "2026-01-01T00:00:01Z",
                    "sessionId": "s1",
                    "message": {"content": [{"type": "text", "text": "history final"}]},
                },
            ],
        )
        registry.add_event(
            "chat_turn",
            {
                "peer": name,
                "peer_id": peer_id,
                "role": "assistant",
                "text": "live final",
                "session_id": "s1",
                "turn_id": "assistant-turn",
            },
        )
        registry.add_event(
            "chat_turn_delta",
            {
                "peer": name,
                "peer_id": peer_id,
                "role": "assistant",
                "session_id": "s1",
                "turn_id": "streaming-turn",
                "chunk_index": 0,
                "kind": "text",
                "text": "streaming",
            },
        )

        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(f"/peers/{name}/timeline")

        assert res.status_code == 200
        body = res.json()
        assert body["peer_id"] == peer_id
        assert body["peer_name"] == name
        assert [(item["source"], item["kind"], item["text"]) for item in body["items"]] == [
            ("history", "turn", "prompt"),
            ("realtime", "turn", "live final"),
            ("realtime", "delta_group", "streaming"),
        ]
        assert body["items"][1]["session_id"] == "s1"
        assert body["items"][1]["turn_id"] == "assistant-turn"
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_timeline_loads_codex_history_with_status(tmp_path: Path) -> None:
    app, registry, _ = _make_app(tmp_path)
    sessions_root = tmp_path / "codex-sessions"
    peer_path = "/peer/work"
    try:
        _peer_id, name = await registry.allocate_and_register(
            circle="global",
            backend=AgentType.CODEX,
            path=peer_path,
            pane_id=None,
            tmux_session=None,
            metadata={},
            machine="test",
        )
        _write_codex_session(
            sessions_root,
            [
                {
                    "type": "session_meta",
                    "payload": {"id": "codex-session", "cwd": peer_path},
                },
                {
                    "type": "response_item",
                    "timestamp": "2026-05-21T08:00:01Z",
                    "payload": {
                        "type": "message",
                        "role": "user",
                        "content": [{"type": "input_text", "text": "codex prompt"}],
                    },
                },
                {
                    "type": "turn_context",
                    "payload": {"turn_id": "codex-turn", "cwd": peer_path},
                },
                {
                    "type": "response_item",
                    "timestamp": "2026-05-21T08:00:02Z",
                    "payload": {
                        "type": "message",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "codex answer"}],
                    },
                },
            ],
        )

        with patch("repowire.session.history._codex_sessions_dir", return_value=sessions_root):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(f"/peers/{name}/timeline")

        assert res.status_code == 200
        body = res.json()
        assert body["history_status"] == "available"
        assert body["history_backend"] == "codex"
        assert [item["text"] for item in body["items"]] == ["codex prompt", "codex answer"]
        assert body["items"][1]["turn_id"] == "codex-turn"
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_timeline_session_filter_and_limit(tmp_path: Path) -> None:
    app, registry, _ = _make_app(tmp_path)
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
        _write_session(
            projects_root,
            peer_path,
            [
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:00Z",
                    "sessionId": "s1",
                    "message": {"content": "old keep"},
                },
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:01Z",
                    "sessionId": "s1",
                    "message": {"content": "new keep"},
                },
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:02Z",
                    "sessionId": "s2",
                    "message": {"content": "drop"},
                },
            ],
        )
        registry.add_event(
            "chat_turn",
            {
                "peer": name,
                "peer_id": peer_id,
                "role": "assistant",
                "text": "live keep",
                "session_id": "s1",
                "turn_id": "live-turn",
            },
        )

        with patch("repowire.session.history._claude_projects_dir", return_value=projects_root):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
                res = await ac.get(
                    f"/peers/{name}/timeline",
                    params={"session_id": "s1", "limit": 2},
                )

        assert res.status_code == 200
        body = res.json()
        assert body["session_id"] == "s1"
        assert [item["text"] for item in body["items"]] == ["new keep", "live keep"]
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_timeline_unknown_peer_returns_404(tmp_path: Path) -> None:
    app, _, _ = _make_app(tmp_path)
    try:
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as ac:
            res = await ac.get("/peers/ghost/timeline")
        assert res.status_code == 404
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_timeline_resolves_claude_history_from_session_binding(tmp_path: Path) -> None:
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
        _write_session(
            projects_root,
            peer_path,
            [
                {
                    "type": "user",
                    "timestamp": "2026-01-01T00:00:00Z",
                    "sessionId": "bound-s1",
                    "message": {"content": "bound prompt"},
                },
                {
                    "type": "assistant",
                    "uuid": "bound-turn",
                    "timestamp": "2026-01-01T00:00:01Z",
                    "sessionId": "bound-s1",
                    "message": {"content": [{"type": "text", "text": "bound answer"}]},
                },
            ],
        )
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
                    f"/peers/{name}/timeline",
                    params={"session_id": "bound-s1"},
                )

        assert res.status_code == 200
        body = res.json()
        assert [item["text"] for item in body["items"]] == ["bound prompt", "bound answer"]
        assert body["history_source"] == "session_binding"
        assert body["repowire_session_id"] == binding.repowire_session_id
        assert body["binding_status"] == "active"
        assert body["runtime_session_id"] == "bound-s1"
    finally:
        cleanup_deps()


@pytest.mark.anyio
async def test_timeline_resolves_codex_history_from_session_binding(tmp_path: Path) -> None:
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
        _write_codex_session(
            sessions_root,
            [
                {
                    "type": "session_meta",
                    "payload": {"id": "codex-bound-s1", "cwd": peer_path},
                },
                {
                    "type": "response_item",
                    "timestamp": "2026-05-21T08:00:01Z",
                    "payload": {
                        "type": "message",
                        "role": "user",
                        "content": [{"type": "input_text", "text": "codex bound prompt"}],
                    },
                },
                {
                    "type": "turn_context",
                    "payload": {"turn_id": "codex-bound-turn", "cwd": peer_path},
                },
                {
                    "type": "response_item",
                    "timestamp": "2026-05-21T08:00:02Z",
                    "payload": {
                        "type": "message",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "codex bound answer"}],
                    },
                },
            ],
        )
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
                    f"/peers/{name}/timeline",
                    params={"session_id": "codex-bound-s1"},
                )

        assert res.status_code == 200
        body = res.json()
        assert [item["text"] for item in body["items"]] == [
            "codex bound prompt",
            "codex bound answer",
        ]
        assert body["history_source"] == "session_binding"
        assert body["repowire_session_id"] == binding.repowire_session_id
        assert body["runtime_session_id"] == "codex-bound-s1"
    finally:
        cleanup_deps()
