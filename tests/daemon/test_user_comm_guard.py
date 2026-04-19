"""Tests for user-facing peer access guard (beads-8th D2).

Subordinate agents (role=AGENT) must not notify telegram peers (role=SERVICE,
display_name starts with 'telegram') directly — not even with bypass_circle=True.
Orchestrators, services, and human peers are allowed through.
"""
from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import Config
from repowire.daemon.deps import cleanup_deps, init_deps, get_peer_registry
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import health, messages, peers as peers_routes
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import PeerRole, PeerStatus


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_peer(display_name: str, role: PeerRole) -> MagicMock:
    p = MagicMock()
    p.display_name = display_name
    p.role = role
    p.status = PeerStatus.ONLINE
    return p


def _make_fake_registry(monkeypatch, from_peer, to_peer):
    """Patch get_peer_registry in routes.messages with controlled peer objects."""

    async def fake_get_peer(identifier, circle=None):
        if identifier == to_peer.display_name:
            return to_peer
        if identifier == from_peer.display_name:
            return from_peer
        return None

    fake = MagicMock()
    fake.lazy_repair = AsyncMock()
    fake.get_peer = AsyncMock(side_effect=fake_get_peer)
    fake.notify = AsyncMock()

    monkeypatch.setattr(messages, "get_peer_registry", lambda: fake)
    return fake


# ---------------------------------------------------------------------------
# Unit tests
# ---------------------------------------------------------------------------


class TestUserCommGuardUnit:
    async def test_subordinate_to_telegram_blocked(self, monkeypatch):
        """agent → telegram(service) must be rejected with 403."""
        from fastapi import HTTPException

        agent = _make_peer("devops-worker-claude-code", PeerRole.AGENT)
        telegram = _make_peer("telegram-claude-code", PeerRole.SERVICE)
        _make_fake_registry(monkeypatch, from_peer=agent, to_peer=telegram)

        request = MagicMock()
        request.from_peer = agent.display_name
        request.to_peer = telegram.display_name
        request.text = "hello"
        request.bypass_circle = False
        request.circle = None

        with pytest.raises(HTTPException) as exc_info:
            await messages.notify_peer(request, _=None)

        assert exc_info.value.status_code == 403
        assert "User-facing peer access denied" in exc_info.value.detail

    async def test_subordinate_to_telegram_bypass_circle_still_blocked(self, monkeypatch):
        """bypass_circle=True must NOT bypass the user-facing guard."""
        from fastapi import HTTPException

        agent = _make_peer("qa-worker-claude-code", PeerRole.AGENT)
        telegram = _make_peer("telegram-claude-code", PeerRole.SERVICE)
        _make_fake_registry(monkeypatch, from_peer=agent, to_peer=telegram)

        request = MagicMock()
        request.from_peer = agent.display_name
        request.to_peer = telegram.display_name
        request.text = "ping"
        request.bypass_circle = True
        request.circle = None

        with pytest.raises(HTTPException) as exc_info:
            await messages.notify_peer(request, _=None)

        assert exc_info.value.status_code == 403
        assert "User-facing peer access denied" in exc_info.value.detail

    async def test_director_to_telegram_allowed(self, monkeypatch):
        """director(orchestrator) → telegram is allowed."""
        director = _make_peer("director-claude-code", PeerRole.ORCHESTRATOR)
        telegram = _make_peer("telegram-claude-code", PeerRole.SERVICE)
        fake = _make_fake_registry(monkeypatch, from_peer=director, to_peer=telegram)

        request = MagicMock()
        request.from_peer = director.display_name
        request.to_peer = telegram.display_name
        request.text = "relay update"
        request.bypass_circle = False
        request.circle = None

        result = await messages.notify_peer(request, _=None)
        assert result.ok is True
        fake.notify.assert_awaited_once()

    async def test_brain_admin_to_telegram_allowed(self, monkeypatch):
        """brain-admin(service) → telegram is allowed."""
        brain_admin = _make_peer("brain-admin-claude-code", PeerRole.SERVICE)
        telegram = _make_peer("telegram-claude-code", PeerRole.SERVICE)
        fake = _make_fake_registry(monkeypatch, from_peer=brain_admin, to_peer=telegram)

        request = MagicMock()
        request.from_peer = brain_admin.display_name
        request.to_peer = telegram.display_name
        request.text = "system update"
        request.bypass_circle = False
        request.circle = None

        result = await messages.notify_peer(request, _=None)
        assert result.ok is True
        fake.notify.assert_awaited_once()

    async def test_subordinate_to_director_allowed(self, monkeypatch):
        """subordinate(agent) → director(orchestrator) is allowed."""
        agent = _make_peer("devops-worker-claude-code", PeerRole.AGENT)
        director = _make_peer("director-claude-code", PeerRole.ORCHESTRATOR)
        fake = _make_fake_registry(monkeypatch, from_peer=agent, to_peer=director)

        request = MagicMock()
        request.from_peer = agent.display_name
        request.to_peer = director.display_name
        request.text = "task done"
        request.bypass_circle = True
        request.circle = None

        result = await messages.notify_peer(request, _=None)
        assert result.ok is True
        fake.notify.assert_awaited_once()


# ---------------------------------------------------------------------------
# Integration test
# ---------------------------------------------------------------------------


def _make_test_app(tmp_path: Path):
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
    app.include_router(health.router)
    app.include_router(peers_routes.router)
    app.include_router(messages.router)
    app.include_router(spawn_routes.router)
    return app


@pytest.fixture
async def client(tmp_path):
    app = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c
    cleanup_deps()


class TestUserCommGuardIntegration:
    async def test_subordinate_to_telegram_bypass_returns_403(self, client):
        """POST /notify subordinate→telegram with bypass_circle=True → 403."""
        # Register telegram peer with SERVICE role
        r = await client.post("/peers", json={
            "name": "telegram",
            "path": "/tmp/telegram",
            "circle": "global",
            "backend": "claude-code",
            "role": "service",
        })
        assert r.status_code == 200
        telegram_name = r.json()["display_name"]
        assert telegram_name.startswith("telegram")

        # Register subordinate agent peer
        r = await client.post("/peers", json={
            "name": "devops-worker",
            "path": "/tmp/devops-worker",
            "circle": "project-test",
            "backend": "claude-code",
            "role": "agent",
        })
        assert r.status_code == 200
        agent_name = r.json()["display_name"]

        # Subordinate tries to notify telegram with bypass_circle=True
        r = await client.post("/notify", json={
            "from_peer": agent_name,
            "to_peer": telegram_name,
            "text": "sneaky message",
            "bypass_circle": True,
        })
        assert r.status_code == 403
        assert "User-facing peer access denied" in r.json()["detail"]
