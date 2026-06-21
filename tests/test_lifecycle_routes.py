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
            pending_path = log_dir / "pending-query-5.json"
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

    async def test_clears_spawned_pane_ownership(
        self,
        client_and_registry,
        tmp_path,
        monkeypatch,
    ):
        """pane_died must clear the pane id from _SPAWNED_PANE_IDS so a
        future tmux server restart can't reuse it and accidentally match
        an externally-attached peer."""
        from repowire.daemon.routes import spawn as spawn_routes
        from repowire.spawn_ownership import get_spawn_ownership, record_spawn_ownership

        monkeypatch.setattr("repowire.spawn_ownership.OWNERSHIP_PATH", tmp_path / "ownership.json")
        client, _ = client_and_registry
        spawn_routes._SPAWNED_PANE_IDS.add("%5")
        try:
            record_spawn_ownership(
                pane_id="%5",
                path="/tmp/test",
                backend=AgentType.CLAUDE_CODE,
                circle="alpha",
                role="agent",
                display_name="myproject",
                tmux_session="default:test",
                peer_id="repow-dev-abc12345",
            )
            assert get_spawn_ownership("%5") is not None
            r = await client.post(
                "/hooks/lifecycle/pane-died",
                json={"pane_id": "%5"},
            )
            assert r.status_code == 200
            assert "%5" not in spawn_routes._SPAWNED_PANE_IDS
            assert get_spawn_ownership("%5") is None
        finally:
            spawn_routes._SPAWNED_PANE_IDS.discard("%5")

    async def test_clears_spawned_pane_even_without_peer(self, client_and_registry):
        """The cleanup happens even if no peer was registered for the pane —
        e.g. spawned peer crashed before registering."""
        from repowire.daemon.routes import spawn as spawn_routes

        client, _ = client_and_registry
        spawn_routes._SPAWNED_PANE_IDS.add("%88")
        try:
            r = await client.post(
                "/hooks/lifecycle/pane-died",
                json={"pane_id": "%88"},
            )
            assert r.status_code == 200
            assert "%88" not in spawn_routes._SPAWNED_PANE_IDS
        finally:
            spawn_routes._SPAWNED_PANE_IDS.discard("%88")


# -- session-closed --


def _pane(pane_id: str, session: str) -> dict:
    return {
        "pane_id": pane_id,
        "pid": 123,
        "command": "claude",
        "cwd": "/tmp/test",
        "session": session,
        "window": "0",
    }


class TestSessionClosed:
    async def test_batch_offline(self, client_and_registry):
        client, registry = client_and_registry
        p1 = _make_peer(
            peer_id="repow-dev-aaa11111", display_name="proj-a", circle="alpha",
            pane_id="%5",
        )
        p2 = _make_peer(
            peer_id="repow-dev-bbb22222", display_name="proj-b", circle="alpha",
            pane_id="%6",
        )
        p3 = _make_peer(
            peer_id="repow-dev-ccc33333", display_name="proj-c", circle="beta",
            pane_id="%7",
        )
        await registry.register_peer(p1)
        await registry.register_peer(p2)
        await registry.register_peer(p3)

        # tmux confirms the 'alpha' session is gone (its panes %5/%6 absent);
        # only the surviving 'beta' pane remains.
        with patch(
            "repowire.daemon.lifecycle_handler.list_all_panes",
            return_value=[_pane("%7", "beta")],
        ):
            r = await client.post(
                "/hooks/lifecycle/session-closed",
                json={"session_name": "alpha"},
            )
        assert r.status_code == 200

        assert (await registry.get_peer(p1.peer_id)).status == PeerStatus.OFFLINE
        assert (await registry.get_peer(p2.peer_id)).status == PeerStatus.OFFLINE
        # beta peer unaffected
        assert (await registry.get_peer(p3.peer_id)).status == PeerStatus.ONLINE

    async def test_spurious_close_ignored_when_session_still_live(
        self, client_and_registry
    ):
        """The repowire-egt-followup bug: a transient session exit fires
        session-closed with #{session_name} resolved to the SURVIVING session.
        If that session still has live panes, the close is spurious and must
        NOT offline its (alive) peers."""
        client, registry = client_and_registry
        p1 = _make_peer(
            peer_id="repow-dev-aaa11111", display_name="proj-a", circle="default",
            pane_id="%50",
        )
        p2 = _make_peer(
            peer_id="repow-dev-bbb22222", display_name="proj-b", circle="default",
            pane_id="%228",
        )
        await registry.register_peer(p1)
        await registry.register_peer(p2)

        # tmux still reports live panes for 'default' -> spurious close.
        with patch(
            "repowire.daemon.lifecycle_handler.list_all_panes",
            return_value=[_pane("%50", "default"), _pane("%228", "default")],
        ):
            r = await client.post(
                "/hooks/lifecycle/session-closed",
                json={"session_name": "default"},
            )
        assert r.status_code == 200

        # Both stay ONLINE — no false offline, no pane-state wipe.
        assert (await registry.get_peer(p1.peer_id)).status == PeerStatus.ONLINE
        assert (await registry.get_peer(p2.peer_id)).status == PeerStatus.ONLINE

    async def test_inconclusive_probe_does_not_mass_offline(
        self, client_and_registry
    ):
        """tmux unreachable / empty listing is 'can't tell', not 'all dead'.
        Refuse to offline on no evidence."""
        client, registry = client_and_registry
        p1 = _make_peer(
            peer_id="repow-dev-aaa11111", display_name="proj-a", circle="default",
            pane_id="%50",
        )
        await registry.register_peer(p1)

        with patch(
            "repowire.daemon.lifecycle_handler.list_all_panes",
            return_value=[],
        ):
            r = await client.post(
                "/hooks/lifecycle/session-closed",
                json={"session_name": "default"},
            )
        assert r.status_code == 200
        assert (await registry.get_peer(p1.peer_id)).status == PeerStatus.ONLINE

    async def test_session_gone_spares_peer_with_live_pane(
        self, client_and_registry
    ):
        """Even when the session is genuinely gone, a peer whose own pane is
        still live is spared (pane death has its own signal)."""
        client, registry = client_and_registry
        dead = _make_peer(
            peer_id="repow-dev-aaa11111", display_name="proj-a", circle="alpha",
            pane_id="%5",
        )
        alive = _make_peer(
            peer_id="repow-dev-bbb22222", display_name="proj-b", circle="alpha",
            pane_id="%6",
        )
        await registry.register_peer(dead)
        await registry.register_peer(alive)

        # 'alpha' session not in the listing, but %6 survived (moved/reused).
        with patch(
            "repowire.daemon.lifecycle_handler.list_all_panes",
            return_value=[_pane("%6", "other")],
        ):
            r = await client.post(
                "/hooks/lifecycle/session-closed",
                json={"session_name": "alpha"},
            )
        assert r.status_code == 200
        assert (await registry.get_peer(dead.peer_id)).status == PeerStatus.OFFLINE
        assert (await registry.get_peer(alive.peer_id)).status == PeerStatus.ONLINE

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
    async def test_does_not_update_display_name(self, client_and_registry):
        """Window renames must not rewrite peer display_name (would strip backend suffix)."""
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
        assert result.display_name == "old-window"


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
