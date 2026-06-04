"""HTTP route tests for POST /peers/{name}/switch-backend (§4.8)."""

from __future__ import annotations

import socket
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.config.models import AgentType, Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, get_config, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers as peers_routes
from repowire.daemon.routes import spawn as spawn_routes
from repowire.daemon.websocket_transport import WebSocketTransport


def _make_test_app(
    tmp_path: Path,
    commands: dict[AgentType, str] | None = None,
):
    cfg = Config()
    cfg.daemon.spawn.commands = commands or {
        AgentType.CLAUDE_CODE: "claude",
        AgentType.CODEX: "codex",
        AgentType.GEMINI: "gemini",
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
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    app_state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=tracker,
        ask_tracker=ask_tracker,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, app_state)

    app = FastAPI()
    app.include_router(peers_routes.router)
    app.include_router(spawn_routes.router)
    return app, registry, ask_tracker


@pytest.fixture
async def env(tmp_path):
    app, registry, ask_tracker = _make_test_app(tmp_path)
    t = ASGITransport(app=app)
    async with AsyncClient(transport=t, base_url="http://test") as client:
        yield SimpleNamespace(client=client, registry=registry, ask_tracker=ask_tracker)
    cleanup_deps()


async def _register(client, *, name="alpha", backend="claude-code", machine=None,
                    path="/tmp/peer"):
    machine = machine or socket.gethostname()
    r = await client.post("/peers", json={
        "name": name,
        "path": path,
        "circle": "default",
        "backend": backend,
        "machine": machine,
    })
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


def _fake_spawn(display_name: str, tmux_session: str, pane_id: str = "%99"):
    from repowire.spawn import SpawnResult
    return SpawnResult(
        display_name=display_name, tmux_session=tmux_session, pane_id=pane_id,
    )


class TestSwitchBackendRoute:
    async def test_same_host_happy_path(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        bin_dir = tmp_path / "bin"
        bin_dir.mkdir()
        codex_bin = bin_dir / "codex"
        codex_bin.write_text("#!/bin/sh\n", encoding="utf-8")
        codex_bin.chmod(0o755)
        get_config().daemon.spawn.env_path = [str(bin_dir)]
        with patch.object(spawn_routes, "spawn_peer", return_value=_fake_spawn(
            "alpha", "default:alpha", "%101",
        )) as mock_spawn, \
            patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "post_spawn_warmup", new_callable=AsyncMock):
            r = await env.client.post(
                f"/peers/{name}/switch-backend",
                json={"new_backend": "codex"},
            )
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["ok"] is True
        assert body["old_backend"] == "claude-code"
        assert body["new_backend"] == "codex"
        assert body["display_name"] == "alpha"
        assert body["tmux_session"] == "default:alpha"

        # spawn_peer must have been invoked with the codex-mapped command and
        # the peer's original path/circle preserved.
        spawn_cfg = mock_spawn.call_args.args[0]
        assert spawn_cfg.command == "codex"
        assert spawn_cfg.backend is AgentType.CODEX
        assert spawn_cfg.circle == "default"
        assert spawn_cfg.path == str(Path(tmp_path).resolve())
        assert spawn_cfg.env == {"PATH": str(bin_dir)}

        # Registry must have unregistered the old peer.
        assert await env.registry.get_peer(name) is None

    async def test_same_backend_returns_409(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        r = await env.client.post(
            f"/peers/{name}/switch-backend",
            json={"new_backend": "claude-code"},
        )
        assert r.status_code == 409
        assert r.json()["detail"]["error"] == "same_backend"

    async def test_missing_command_returns_422(self, tmp_path):
        # Build an app whose configured commands have no entry for gemini.
        app, _registry, _ask_tracker = _make_test_app(
            tmp_path,
            commands={AgentType.CLAUDE_CODE: "claude", AgentType.CODEX: "codex"},
        )
        try:
            transport = ASGITransport(app=app)
            async with AsyncClient(transport=transport, base_url="http://test") as client:
                name = await _register(client, name="alpha", backend="claude-code",
                                        path=str(tmp_path))
                r = await client.post(
                    f"/peers/{name}/switch-backend",
                    json={"new_backend": "gemini"},
                )
            assert r.status_code == 422
            body = r.json()
            assert body["detail"]["error"] == "command_unavailable"
            assert "daemon.spawn.commands" in body["detail"]["hint"]
            assert body["detail"]["new_backend"] == "gemini"
        finally:
            cleanup_deps()

    async def test_cross_host_returns_409(self, env, tmp_path):
        name = await _register(
            env.client, name="alpha", backend="claude-code",
            machine="other-host.example", path=str(tmp_path),
        )
        r = await env.client.post(
            f"/peers/{name}/switch-backend",
            json={"new_backend": "codex"},
        )
        assert r.status_code == 409
        assert r.json()["detail"]["error"] == "cross_host"

    async def test_in_flight_ask_blocks_switch(self, env, tmp_path):
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        peer = await env.registry.get_peer(name)
        assert peer is not None

        # Register an open ask targeting this peer.
        await env.ask_tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text="hello",
        )

        with patch.object(spawn_routes, "spawn_peer") as mock_spawn:
            r = await env.client.post(
                f"/peers/{name}/switch-backend",
                json={"new_backend": "codex"},
            )
        assert r.status_code == 409
        detail = r.json()["detail"]
        assert detail["error"] == "in_flight_asks"
        assert len(detail["open_asks"]) == 1
        mock_spawn.assert_not_called()
        # Peer must still exist (no kill on best-effort failure path).
        assert await env.registry.get_peer(name) is not None

    async def test_unknown_peer_returns_404(self, env):
        r = await env.client.post(
            "/peers/nonexistent/switch-backend",
            json={"new_backend": "codex"},
        )
        assert r.status_code == 404

    async def test_concurrent_switch_returns_409_switch_in_progress(self, env, tmp_path):
        """A second switch for the same peer while the first is mid-flight
        must be refused, not silently piggy-back on the same barrier."""
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        peer = await env.registry.get_peer(name)
        assert peer is not None

        # Pre-acquire the barrier to simulate an in-progress switch.
        await env.ask_tracker.begin_quiesce(peer.peer_id)
        try:
            with patch.object(spawn_routes, "spawn_peer") as mock_spawn, \
                patch.object(spawn_routes, "kill_pane", return_value=True):
                r = await env.client.post(
                    f"/peers/{name}/switch-backend",
                    json={"new_backend": "codex"},
                )
            assert r.status_code == 409
            assert r.json()["detail"]["error"] == "switch_in_progress"
            mock_spawn.assert_not_called()
            # Peer must still exist; we didn't enter the kill section.
            assert await env.registry.get_peer(name) is not None
        finally:
            await env.ask_tracker.end_quiesce(peer.peer_id)

    async def test_kill_pane_failure_aborts_switch(self, env, tmp_path):
        """If kill_pane returns False, abort and leave the peer registered.

        Otherwise we'd unregister a peer whose underlying agent is still alive,
        leaving a zombie runtime.
        """
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        peer = await env.registry.get_peer(name)
        assert peer is not None
        # Force the daemon to think it owns the pane so kill_pane is called.
        pane_id = "%9999"
        peer.pane_id = pane_id
        spawn_routes._SPAWNED_PANE_IDS.add(pane_id)
        try:
            with patch.object(spawn_routes, "spawn_peer") as mock_spawn, \
                patch.object(spawn_routes, "kill_pane", return_value=False) as mock_kill:
                r = await env.client.post(
                    f"/peers/{name}/switch-backend",
                    json={"new_backend": "codex"},
                )
            assert r.status_code == 500
            assert r.json()["detail"]["error"] == "kill_failed"
            mock_kill.assert_called_once_with(pane_id)
            mock_spawn.assert_not_called()
            # Peer must still be registered — we refused to deregister a live
            # runtime.
            assert await env.registry.get_peer(name) is not None
            # Quiesce barrier must be released so subsequent asks aren't blocked
            # forever.
            assert peer.peer_id not in env.ask_tracker._quiescing
        finally:
            spawn_routes._SPAWNED_PANE_IDS.discard(pane_id)

    async def test_concurrent_ask_blocked_during_switch(self, env, tmp_path):
        """A /ask landing mid-switch must be refused so it can't be orphaned.

        Simulates the race codex flagged: pre-check passes, then a new ask
        arrives during the kill+respawn window. spawn_peer is synchronous, so
        we use it as the synchronization point — while it runs, the barrier is
        held, and an /ask issued just before the switch but processed during
        spawn must be rejected.
        """
        name = await _register(env.client, name="alpha", backend="claude-code",
                                path=str(tmp_path))
        peer = await env.registry.get_peer(name)
        assert peer is not None

        racy_ask_result: dict[str, object] = {}

        def fake_spawn(_cfg):
            # While spawn_peer runs, the quiesce barrier is held. Verify a
            # concurrent register() would be rejected by inspecting the
            # in-process barrier set (the same check register() does under
            # the lock).
            try:
                from repowire.daemon.ask_tracker import QuiescedError
                if peer.peer_id in env.ask_tracker._quiescing:
                    raise QuiescedError(peer.peer_id)
                racy_ask_result["error"] = "no_barrier"
            except Exception as e:
                racy_ask_result["error"] = type(e).__name__
            from repowire.spawn import SpawnResult
            return SpawnResult(
                display_name="alpha", tmux_session="default:alpha", pane_id="%200",
            )

        with patch.object(spawn_routes, "spawn_peer", side_effect=fake_spawn), \
            patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(spawn_routes, "post_spawn_warmup", new_callable=AsyncMock):
            r = await env.client.post(
                f"/peers/{name}/switch-backend",
                json={"new_backend": "codex"},
            )
        assert r.status_code == 200, r.text
        # The racing register() would have been rejected by the barrier.
        assert racy_ask_result.get("error") == "QuiescedError"
        # Barrier must have been released after the switch completed.
        assert peer.peer_id not in env.ask_tracker._quiescing


class TestAskTrackerQuiesceBarrier:
    """Direct unit tests for the AskTracker switch barrier."""

    async def test_register_rejected_while_quiescing(self):
        from repowire.daemon.ask_tracker import AskTracker, QuiescedError
        tracker = AskTracker()
        peer_id = "repow-default-aa11bb22"
        await tracker.begin_quiesce(peer_id)
        with pytest.raises(QuiescedError):
            await tracker.register(
                from_peer_id="dashboard",
                from_peer_name="dashboard",
                to_peer_id=peer_id,
                to_peer_name="alpha",
                text="should be refused",
            )
        await tracker.end_quiesce(peer_id)
        # After release, register succeeds.
        cid = await tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer_id,
            to_peer_name="alpha",
            text="now allowed",
        )
        assert cid

    async def test_begin_quiesce_fails_with_open_asks(self):
        from repowire.daemon.ask_tracker import AskTracker, QuiesceFailedError
        tracker = AskTracker()
        peer_id = "repow-default-aa11bb22"
        cid = await tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer_id,
            to_peer_name="alpha",
            text="open ask",
        )
        with pytest.raises(QuiesceFailedError) as ei:
            await tracker.begin_quiesce(peer_id)
        assert cid in ei.value.open_cids
        # No barrier acquired on failure → second register still works.
        cid2 = await tracker.register(
            from_peer_id="dashboard",
            from_peer_name="dashboard",
            to_peer_id=peer_id,
            to_peer_name="alpha",
            text="still open",
        )
        assert cid2 != cid

    async def test_begin_quiesce_is_exclusive(self):
        """Two begin_quiesce calls for the same peer must not both succeed.

        Otherwise two concurrent switches could both enter the critical
        section, and the first end_quiesce would prematurely release the
        barrier for the second.
        """
        from repowire.daemon.ask_tracker import AskTracker, QuiesceFailedError
        tracker = AskTracker()
        peer_id = "repow-default-aa11bb22"
        await tracker.begin_quiesce(peer_id)
        with pytest.raises(QuiesceFailedError) as ei:
            await tracker.begin_quiesce(peer_id)
        # Empty open_cids signals "already quiescing" vs "had open asks".
        assert ei.value.open_cids == []
        # A different peer can still acquire its own barrier.
        await tracker.begin_quiesce("repow-default-cc33dd44")
        await tracker.end_quiesce(peer_id)
        # After release, re-acquire works.
        await tracker.begin_quiesce(peer_id)

    async def test_quiesce_blocks_outbound_too(self):
        """A peer that is itself asking shouldn't have its outbound asks
        accepted mid-switch either — the asker would die before getting a
        reply."""
        from repowire.daemon.ask_tracker import AskTracker, QuiescedError
        tracker = AskTracker()
        peer_id = "repow-default-aa11bb22"
        await tracker.begin_quiesce(peer_id)
        with pytest.raises(QuiescedError):
            await tracker.register(
                from_peer_id=peer_id,
                from_peer_name="alpha",
                to_peer_id="dashboard",
                to_peer_name="dashboard",
                text="outbound during switch",
            )


class TestCommandForBackend:
    def test_returns_configured_backend_command(self, tmp_path):
        _app, _r, _a = _make_test_app(
            tmp_path,
            commands={
                AgentType.CLAUDE_CODE: "claude --dangerously-skip-permissions",
                AgentType.CODEX: "codex",
            },
        )
        try:
            assert (
                spawn_routes._command_for_backend(AgentType.CLAUDE_CODE)
                == "claude --dangerously-skip-permissions"
            )
            assert spawn_routes._command_for_backend(AgentType.CODEX) == "codex"
            assert spawn_routes._command_for_backend(AgentType.GEMINI) is None
        finally:
            cleanup_deps()
