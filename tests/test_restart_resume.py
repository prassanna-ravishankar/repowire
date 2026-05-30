"""Tests for durable-resume restart (t0p): restart resumes the backend session
when a valid runtime_session_id is on disk, else fresh + warning."""

from __future__ import annotations

import socket
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.spawn import SpawnResult


def _make_app(tmp_path: Path):
    cfg = Config()
    cfg.daemon.spawn.commands = {
        AgentType.CLAUDE_CODE: "claude --dangerously-skip-permissions",
        AgentType.CODEX: "codex --dangerously-bypass-approvals-and-sandbox",
    }
    cfg.daemon.spawn.allowed_paths = ["/"]
    transport = WebSocketTransport()
    tracker = QueryTracker()
    ask_tracker = AskTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        ask_tracker=ask_tracker,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600
    state_db = StateDatabase(tmp_path / "state.db")
    binding_store = SQLiteSessionBindingStore(state_db)
    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        ask_tracker=ask_tracker,
        message_router=router,
        peer_registry=registry,
        session_binding_store=binding_store,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state)
    app = FastAPI()
    app.include_router(peers_routes.router)
    app.include_router(spawn_routes.router)
    return app, registry, binding_store


@pytest.fixture
async def env(tmp_path, monkeypatch):
    spawn_routes._SPAWNED_PANE_IDS.clear()
    monkeypatch.setattr("repowire.spawn_ownership.OWNERSHIP_PATH", tmp_path / "ownership.json")
    app, registry, binding_store = _make_app(tmp_path)
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        yield SimpleNamespace(
            client=client, registry=registry, binding_store=binding_store, tmp_path=tmp_path
        )
    spawn_routes._SPAWNED_PANE_IDS.clear()
    cleanup_deps()


async def _register(
    client, *, path: str, backend: str = "claude-code", pane_id: str = "%101"
) -> dict:
    r = await client.post(
        "/peers",
        json={
            "name": Path(path).name,
            "path": path,
            "circle": "default",
            "backend": backend,
            "machine": socket.gethostname(),
            "role": "agent",
            "pane_id": pane_id,
        },
    )
    assert r.status_code == 200, r.text
    rr = await client.get(f"/peers/{r.json()['display_name']}")
    return rr.json()


def _spawn_result(pane_id: str = "%202") -> SpawnResult:
    return SpawnResult(display_name="proj", tmux_session="default:proj", pane_id=pane_id)


def _bind(store, *, peer_id, backend, project_path, runtime_session_id):
    store.upsert_observation(
        peer_id=peer_id,
        backend=backend,
        project_path=project_path,
        runtime_session_id=runtime_session_id,
    )


def _write_claude_session(home: Path, cwd: str, session_id: str) -> None:
    # ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl
    enc = cwd.replace("/", "-")
    d = home / ".claude" / "projects" / enc
    d.mkdir(parents=True, exist_ok=True)
    (d / f"{session_id}.jsonl").write_text('{"type":"summary"}\n')


class TestRestartResume:
    async def test_resumes_when_session_on_disk(self, env, tmp_path, monkeypatch):
        home = tmp_path / "home"
        monkeypatch.setenv("HOME", str(home))
        monkeypatch.setattr(Path, "home", classmethod(lambda cls: home))
        path = str(tmp_path / "proj")
        Path(path).mkdir(parents=True, exist_ok=True)
        info = await _register(env.client, path=path, pane_id="%101")
        sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
        _write_claude_session(home, str(Path(path).resolve()), sid)
        _bind(env.binding_store, peer_id=info["peer_id"], backend="claude-code",
              project_path=str(Path(path).resolve()), runtime_session_id=sid)
        spawn_routes._SPAWNED_PANE_IDS.add("%101")

        captured = {}
        def fake_spawn(cfg):
            captured["command"] = cfg.command
            return _spawn_result()
        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "spawn_peer", side_effect=fake_spawn), \
            patch.object(spawn_routes, "post_spawn_warmup"):
            r = await env.client.post(f"/peers/{info['display_name']}/restart", json={})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["resume_mode"] == "resumed"
        assert body["resume_warning"] is None
        assert f"--resume {sid}" in captured["command"]

    async def test_fresh_when_no_session_id(self, env, tmp_path):
        path = str(tmp_path / "proj2")
        Path(path).mkdir(parents=True, exist_ok=True)
        info = await _register(env.client, path=path, pane_id="%101")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        # No binding -> no session id
        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "spawn_peer", return_value=_spawn_result()), \
            patch.object(spawn_routes, "post_spawn_warmup"):
            r = await env.client.post(f"/peers/{info['display_name']}/restart", json={})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["resume_mode"] == "fresh_runtime_context"
        assert "no captured backend session id" in body["resume_warning"]

    async def test_fresh_when_session_id_stale(self, env, tmp_path):
        # Binding has an id, but NO session file on disk -> must not try to resume
        # (would die hard), restart fresh with a warning instead.
        path = str(tmp_path / "proj3")
        Path(path).mkdir(parents=True, exist_ok=True)
        info = await _register(env.client, path=path, pane_id="%101")
        _bind(env.binding_store, peer_id=info["peer_id"], backend="claude-code",
              project_path=str(Path(path).resolve()),
              runtime_session_id="dead-beef-no-file-on-disk")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "spawn_peer", return_value=_spawn_result()), \
            patch.object(spawn_routes, "post_spawn_warmup"):
            r = await env.client.post(f"/peers/{info['display_name']}/restart", json={})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["resume_mode"] == "fresh_runtime_context"
        assert "no local session file" in body["resume_warning"]

    async def test_stale_id_with_other_session_in_cwd_does_not_resume(
        self, env, tmp_path, monkeypatch
    ):
        # Regression (codex): a stale binding id + a DIFFERENT Claude session file
        # in the same cwd must NOT pass validation — otherwise we'd build
        # --resume <stale-id>, kill the pane, and Claude would exit hard.
        home = tmp_path / "home5"
        monkeypatch.setenv("HOME", str(home))
        monkeypatch.setattr(Path, "home", classmethod(lambda cls: home))
        path = str(tmp_path / "proj5")
        Path(path).mkdir(parents=True, exist_ok=True)
        info = await _register(env.client, path=path, pane_id="%101")
        # A different (unrelated) session file exists in the cwd...
        _write_claude_session(home, str(Path(path).resolve()), "other-session-uuid-9999")
        # ...but the binding points at a stale id with NO file of its own.
        _bind(env.binding_store, peer_id=info["peer_id"], backend="claude-code",
              project_path=str(Path(path).resolve()),
              runtime_session_id="stale-target-uuid-0000")
        spawn_routes._SPAWNED_PANE_IDS.add("%101")
        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "spawn_peer", return_value=_spawn_result()), \
            patch.object(spawn_routes, "post_spawn_warmup"):
            r = await env.client.post(f"/peers/{info['display_name']}/restart", json={})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["resume_mode"] == "fresh_runtime_context"
        assert "no local session file" in body["resume_warning"]

    async def test_non_owned_resumable_refusal_includes_resume_command(
        self, env, tmp_path, monkeypatch
    ):
        home = tmp_path / "home4"
        monkeypatch.setenv("HOME", str(home))
        monkeypatch.setattr(Path, "home", classmethod(lambda cls: home))
        path = str(tmp_path / "proj4")
        Path(path).mkdir(parents=True, exist_ok=True)
        info = await _register(env.client, path=path, pane_id="%900")
        sid = "11111111-2222-3333-4444-555555555555"
        _write_claude_session(home, str(Path(path).resolve()), sid)
        _bind(env.binding_store, peer_id=info["peer_id"], backend="claude-code",
              project_path=str(Path(path).resolve()), runtime_session_id=sid)
        # NOT in _SPAWNED_PANE_IDS -> ownership refusal
        r = await env.client.post(f"/peers/{info['display_name']}/restart", json={})
        assert r.status_code == 409, r.text
        detail = r.json()["detail"]
        assert detail["error"] == "unsupported_pane_ownership"
        assert detail.get("resume_mode") == "manual_resume_available"
        assert f"--resume {sid}" in detail.get("resume_command", "")
