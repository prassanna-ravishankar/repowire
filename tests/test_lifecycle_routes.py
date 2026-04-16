"""Tests for lifecycle event HTTP endpoints."""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.lifecycle_handler import LifecycleHandler
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import lifecycle, peers
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import Peer, PeerStatus


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

    handler = LifecycleHandler(
        peer_registry=registry,
        query_tracker=tracker,
        transport=transport,
    )

    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state, lifecycle_handler=handler)

    app = FastAPI()
    app.include_router(lifecycle.router)
    app.include_router(peers.router)
    return app, registry


def _make_peer(
    peer_id: str = "repow-dev-abc12345",
    display_name: str = "myproject",
    circle: str = "alpha",
    pane_id: str | None = "%5",
) -> Peer:
    return Peer(
        peer_id=peer_id,
        display_name=display_name,
        path="/tmp/test",
        machine="test",
        backend=AgentType.CLAUDE_CODE,
        circle=circle,
        status=PeerStatus.ONLINE,
        pane_id=pane_id,
    )


@pytest.fixture
async def client_and_registry(tmp_path):
    app, registry = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as c:
        yield c, registry
    cleanup_deps()


# -- pane-died --


class TestPaneDied:
    async def test_marks_peer_offline(self, client_and_registry):
        client, registry = client_and_registry
        peer = _make_peer(pane_id="%5")
        await registry.register_peer(peer)

        r = await client.post(
            "/hooks/lifecycle/pane-died",
            json={"pane_id": "%5"},
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True

        result = await registry.get_peer(peer.peer_id)
        assert result.status == PeerStatus.OFFLINE

    async def test_clears_pending_pane_state(self, client_and_registry, tmp_path):
        client, registry = client_and_registry
        peer = _make_peer(pane_id="%5")
        await registry.register_peer(peer)

        with patch("repowire.config.models.CACHE_DIR", tmp_path):
            log_dir = tmp_path / "logs"
            log_dir.mkdir(parents=True, exist_ok=True)
            pending_path = log_dir / "pending-5.json"
            pending_path.write_text('["cid-1"]')

            r = await client.post(
                "/hooks/lifecycle/pane-died",
                json={"pane_id": "%5"},
            )

            assert r.status_code == 200
            assert not pending_path.exists()

    async def test_unknown_pane_is_ok(self, client_and_registry):
        client, _ = client_and_registry
        r = await client.post(
            "/hooks/lifecycle/pane-died",
            json={"pane_id": "%99"},
        )
        assert r.status_code == 200


# -- session-closed --


class TestSessionClosed:
    async def test_batch_offline(self, client_and_registry):
        client, registry = client_and_registry
        p1 = _make_peer(peer_id="repow-dev-aaa11111", display_name="proj-a", circle="alpha")
        p2 = _make_peer(peer_id="repow-dev-bbb22222", display_name="proj-b", circle="alpha")
        p3 = _make_peer(peer_id="repow-dev-ccc33333", display_name="proj-c", circle="beta")
        await registry.register_peer(p1)
        await registry.register_peer(p2)
        await registry.register_peer(p3)

        r = await client.post(
            "/hooks/lifecycle/session-closed",
            json={"session_name": "alpha"},
        )
        assert r.status_code == 200

        assert (await registry.get_peer(p1.peer_id)).status == PeerStatus.OFFLINE
        assert (await registry.get_peer(p2.peer_id)).status == PeerStatus.OFFLINE
        # beta peer unaffected
        assert (await registry.get_peer(p3.peer_id)).status == PeerStatus.ONLINE

    async def test_unknown_session_is_ok(self, client_and_registry):
        client, _ = client_and_registry
        r = await client.post(
            "/hooks/lifecycle/session-closed",
            json={"session_name": "nonexistent"},
        )
        assert r.status_code == 200


# -- session-renamed --


class TestSessionRenamed:
    async def test_updates_circle(self, client_and_registry):
        client, registry = client_and_registry
        peer = _make_peer(circle="old-name", pane_id="%5")
        await registry.register_peer(peer)

        r = await client.post(
            "/hooks/lifecycle/session-renamed",
            json={"new_name": "new-name", "pane_ids": ["%5"]},
        )
        assert r.status_code == 200

        result = await registry.get_peer(peer.peer_id)
        assert result.circle == "new-name"

    async def test_only_matching_panes(self, client_and_registry):
        client, registry = client_and_registry
        p1 = _make_peer(
            peer_id="repow-dev-aaa11111", display_name="proj-a",
            pane_id="%5", circle="alpha",
        )
        p2 = _make_peer(
            peer_id="repow-dev-bbb22222", display_name="proj-b",
            pane_id="%6", circle="alpha",
        )
        await registry.register_peer(p1)
        await registry.register_peer(p2)

        r = await client.post(
            "/hooks/lifecycle/session-renamed",
            json={"new_name": "beta", "pane_ids": ["%5"]},
        )
        assert r.status_code == 200

        assert (await registry.get_peer(p1.peer_id)).circle == "beta"
        assert (await registry.get_peer(p2.peer_id)).circle == "alpha"


# -- window-renamed --


class TestWindowRenamed:
    async def test_updates_display_name(self, client_and_registry):
        client, registry = client_and_registry
        peer = _make_peer(display_name="old-window", circle="alpha", pane_id="%5")
        await registry.register_peer(peer)

        r = await client.post(
            "/hooks/lifecycle/window-renamed",
            json={
                "session_name": "alpha",
                "new_name": "new-window",
                "pane_ids": ["%5"],
            },
        )
        assert r.status_code == 200

        result = await registry.get_peer(peer.peer_id)
        assert result.display_name == "new-window"


# -- client-detached --


class TestClientDetached:
    async def test_is_noop(self, client_and_registry):
        client, registry = client_and_registry
        peer = _make_peer(circle="alpha")
        await registry.register_peer(peer)

        r = await client.post(
            "/hooks/lifecycle/client-detached",
            json={"session_name": "alpha"},
        )
        assert r.status_code == 200

        # Peer stays online (detach is logged, not acted on)
        result = await registry.get_peer(peer.peer_id)
        assert result.status == PeerStatus.ONLINE
