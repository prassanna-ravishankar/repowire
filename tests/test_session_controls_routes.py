"""HTTP tests for session-targeted control routes."""

from __future__ import annotations

import pytest
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon import app as app_mod
from repowire.daemon.peer_delivery import NotifyDeliveryResult

pytestmark = pytest.mark.anyio


async def _register_bound_peer(client: AsyncClient) -> str:
    response = await client.post(
        "/peers",
        json={
            "name": "worker",
            "path": "/repo",
            "circle": "default",
            "backend": "claude-code",
            "metadata": {
                "hook_session_id": "runtime-active-1",
                "runtime_source_uri": "claude-jsonl:repo/runtime-active-1.jsonl",
            },
        },
    )
    assert response.status_code == 200, response.text
    return response.json()["peer_id"]


class _FakePeerDelivery:
    def __init__(self) -> None:
        self.calls: list[dict[str, object]] = []

    async def notify_result(self, **kwargs):
        self.calls.append(kwargs)
        return NotifyDeliveryResult(
            status="sent",
            delivery_state="delivered",
            reason="transport_delivered",
            from_peer_id=None,
            from_peer_name=kwargs["from_peer"],
            to_peer_id=kwargs["to_peer"],
            to_peer_name="worker-claude-code",
        )


async def test_session_resume_returns_active_executor(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            peer_id = await _register_bound_peer(client)
            binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-active-1",
                backend="claude-code",
                project_path="/repo",
            )
            assert binding is not None

            response = await client.post(
                f"/sessions/{binding.repowire_session_id}/controls/resume",
                json={},
            )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["status"] == "active_executor"
    assert body["capability"] == "active_executor"
    assert body["executor_peer_id"] == peer_id
    assert body["runtime_session_id"] == "runtime-active-1"


async def test_session_resume_reports_supported_backend_capability(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        binding = app.state.session_binding_store.upsert_observation(
            peer_id=None,
            backend="codex",
            project_path="/repo",
            runtime_session_id="codex-runtime-1",
            runtime_source_uri="codex-rollout:repo/codex-runtime-1.jsonl",
            resume_capability={
                "supported": True,
                "strategy": "codex_resume",
                "runtime_session_id_arg": "codex-runtime-1",
            },
            status="resumable",
        )
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                f"/sessions/{binding.repowire_session_id}/controls/resume",
                json={},
            )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["status"] == "resume_available"
    assert body["capability"] == "supported"
    assert body["backend"] == "codex"
    assert body["resume_capability"]["strategy"] == "codex_resume"


async def test_session_resume_reports_unsupported_fallback(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        binding = app.state.session_binding_store.upsert_observation(
            peer_id=None,
            backend="gemini",
            project_path="/repo",
            runtime_session_id="gemini-runtime-1",
            status="detached",
        )
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                f"/sessions/{binding.repowire_session_id}/controls/resume",
                json={},
            )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["status"] == "unsupported"
    assert body["capability"] == "unsupported"
    assert "No compatible backend resume capability" in body["message"]


async def test_session_notify_targets_active_executor(tmp_path):
    cfg = Config(experiments={"sqlite_state": True})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        fake_delivery = _FakePeerDelivery()
        app.state.peer_delivery = fake_delivery
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            peer_id = await _register_bound_peer(client)
            binding = app.state.session_binding_store.get_by_runtime_session(
                "runtime-active-1",
                backend="claude-code",
                project_path="/repo",
            )
            assert binding is not None
            response = await client.post(
                f"/sessions/{binding.repowire_session_id}/controls/notify",
                json={"from_peer": "dashboard", "text": "continue this work"},
            )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["capability"] == "active_executor"
    assert body["executor_peer_id"] == peer_id
    assert body["delivery_state"] == "delivered"
    assert fake_delivery.calls == [
        {
            "from_peer": "dashboard",
            "to_peer": peer_id,
            "text": "continue this work",
            "bypass_circle": True,
            "attachments": [],
        }
    ]


async def test_session_controls_require_binding_store(tmp_path):
    cfg = Config(experiments={"sqlite_state": False})
    app = app_mod.create_test_app(config=cfg, persistence_path=tmp_path / "sessions.json")

    async with app.router.lifespan_context(app):
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/sessions/rw-session-missing/controls/resume", json={})

    assert response.status_code == 422
    assert response.json()["detail"]["error"] == "session_bindings_unavailable"
