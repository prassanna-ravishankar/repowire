"""Orphan-pane discovery + link flow (repowire-xqq).

Discovery lists every unregistered tmux pane with a display-only backend hint.
Link is fail-closed: a peer is adopted ONLY when a live ws-hook transport is
observed; a link that can't connect rolls the registration back (no ghost).
"""

from __future__ import annotations

import pytest

from repowire.daemon import orphan_panes
from repowire.daemon.routes import peers as peers_routes
from repowire.hooks._tmux import PaneInfo

from .conftest import async_client_for, make_daemon_app


def _pane(pane_id: str, command: str) -> PaneInfo:
    return PaneInfo(
        pane_id=pane_id, pid=1234, command=command, cwd="/tmp/x", session="s", window="0"
    )


# --- detection (display-only hint) ---


@pytest.mark.parametrize(
    "command,backend,confidence",
    [
        ("claude", "claude-code", "hint"),
        ("/usr/local/bin/codex", "codex", "hint"),
        ("gemini", "gemini", "hint"),
        ("opencode", "opencode", "hint"),
        ("zsh", "unknown", "unknown"),
        ("vim", "unknown", "unknown"),
    ],
)
def test_detect_backend_is_a_hint(command, backend, confidence):
    assert orphan_panes.detect_backend(command) == (backend, confidence)


def test_find_orphans_excludes_registered_and_lists_everything(monkeypatch):
    monkeypatch.setattr(
        orphan_panes,
        "list_all_panes",
        lambda: [
            _pane("%1", "claude"),
            _pane("%2", "zsh"),  # not an agent, still listed (no filtering)
            _pane("%3", "codex"),
        ],
    )
    orphans = orphan_panes.find_orphan_panes(registered_pane_ids={"%3"})
    ids = [o["pane_id"] for o in orphans]
    assert ids == ["%1", "%2"]  # %3 registered → excluded; %2 kept despite non-agent
    by_id = {o["pane_id"]: o for o in orphans}
    assert by_id["%1"]["detected_backend"] == "claude-code"
    assert by_id["%2"]["detected_backend"] == "unknown"


# --- GET /panes/orphans ---


@pytest.mark.asyncio
async def test_orphans_route_excludes_registered_peer_panes(tmp_path, monkeypatch):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    # Register a peer holding pane %1 so it's excluded.
    from repowire.protocol.peers import Peer

    await harness.registry.register_peer(
        Peer(peer_id="p1", display_name="alice", pane_id="%1", path="/tmp/a", machine="h")
    )
    monkeypatch.setattr(
        orphan_panes,
        "list_all_panes",
        lambda: [_pane("%1", "claude"), _pane("%2", "codex")],
    )
    async with async_client_for(harness.app) as client:
        resp = await client.get("/panes/orphans")
    assert resp.status_code == 200
    panes = resp.json()["panes"]
    assert [p["pane_id"] for p in panes] == ["%2"]
    assert panes[0]["detected_backend"] == "codex"
    assert panes[0]["confidence"] == "hint"


# --- link_spawn_ws_hook exception safety ---


def test_link_spawn_ws_hook_swallows_spawn_errors(tmp_path, monkeypatch):
    # If spawn_ws_hook raises after the metadata write, the helper must catch it
    # and return False (so the route's rollback runs) rather than propagating.
    from repowire.hooks import ws_hook_supervisor as sup

    monkeypatch.setattr(sup, "ws_hook_lock_path", lambda _pid: tmp_path / "lock")
    monkeypatch.setattr(sup, "write_pane_runtime_metadata", lambda *_a, **_k: None)

    def boom(**_kw):
        raise RuntimeError("popen exploded")

    monkeypatch.setattr(sup, "spawn_ws_hook", boom)
    result = sup.link_spawn_ws_hook(
        "%99", peer_id="p", display_name="d", backend="codex", cwd="/tmp/x"
    )
    assert result is False


# --- POST /panes/{id}/link ---


@pytest.mark.asyncio
async def test_link_rejects_bad_pane_id(tmp_path):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    async with async_client_for(harness.app) as client:
        resp = await client.post("/panes/not-a-pane/link", json={"backend": "claude-code"})
    assert resp.status_code == 422


@pytest.mark.asyncio
async def test_link_succeeds_when_ws_connects(tmp_path, monkeypatch):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    spawn_calls: list[dict] = []

    def fake_spawn(pane_id, **kw):
        spawn_calls.append({"pane_id": pane_id, **kw})
        return True

    monkeypatch.setattr(peers_routes, "link_spawn_ws_hook", fake_spawn)
    # Transport reports the freshly-linked peer as connected.
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: True)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%2542/link", json={"backend": "claude-code", "cwd": "/tmp/proj"}
        )
    assert resp.status_code == 200
    body = resp.json()
    assert body["linked"] is True
    assert body["transport_connected"] is True
    # The first-adoption spawn helper ran with the pane + identity metadata —
    # NOT maybe_respawn (which would no-op on an orphan with no pidfile).
    assert spawn_calls and spawn_calls[0]["pane_id"] == "%42"
    assert spawn_calls[0]["backend"] == "claude-code"
    assert spawn_calls[0]["cwd"] == "/tmp/proj"
    # The peer is in the roster bound to the pane.
    peer = await harness.registry.get_peer_by_pane("%42")
    assert peer is not None


@pytest.mark.asyncio
async def test_link_rolls_back_when_ws_never_connects(tmp_path, monkeypatch):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    monkeypatch.setattr(peers_routes, "link_spawn_ws_hook", lambda *a, **k: True)
    monkeypatch.setattr(peers_routes, "clear_pane_runtime_state", lambda *a, **k: None)
    # WS never connects → fail-closed, no ghost left behind.
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: False)
    monkeypatch.setattr(peers_routes, "_LINK_WS_WAIT_SECONDS", 0.05)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%2543/link", json={"backend": "codex", "cwd": "/tmp/proj"}
        )
    assert resp.status_code == 200
    body = resp.json()
    assert body["linked"] is False
    assert body["transport_connected"] is False
    assert body["reason"] == "transport_unestablished"
    assert "repowire link --pane %43" in body["repair_hint"]
    # Rolled back: no ghost peer for the pane.
    assert await harness.registry.get_peer_by_pane("%43") is None


@pytest.mark.asyncio
async def test_link_fails_when_spawn_helper_cannot_start(tmp_path, monkeypatch):
    # A pane whose ws-hook can't be spawned (lock contested / script missing)
    # must not be reported linked, and must leave no ghost.
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    monkeypatch.setattr(peers_routes, "link_spawn_ws_hook", lambda *a, **k: False)
    monkeypatch.setattr(peers_routes, "clear_pane_runtime_state", lambda *a, **k: None)
    is_connected_calls: list[str] = []
    monkeypatch.setattr(
        harness.transport,
        "is_connected",
        lambda pid: is_connected_calls.append(pid) or False,
    )

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%2544/link", json={"backend": "codex", "cwd": "/tmp/proj"}
        )
    body = resp.json()
    assert body["linked"] is False
    assert body["reason"] == "ws_hook_spawn_failed"
    # Spawn failed → we never even poll for a connection.
    assert is_connected_calls == []
    assert await harness.registry.get_peer_by_pane("%44") is None


@pytest.mark.asyncio
async def test_link_resolves_cwd_from_live_pane_when_omitted(tmp_path, monkeypatch):
    # The CLI/dashboard copy command sends only --pane + --backend; the daemon
    # (discovery owner) resolves cwd from the live pane so Popen has a real dir.
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    monkeypatch.setattr(
        peers_routes, "list_all_panes", lambda: [_pane("%50", "claude", )]
    )
    spawn_calls: list[dict] = []
    monkeypatch.setattr(
        peers_routes,
        "link_spawn_ws_hook",
        lambda pane_id, **kw: spawn_calls.append(kw) or True,
    )
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: True)

    async with async_client_for(harness.app) as client:
        resp = await client.post("/panes/%2550/link", json={"backend": "claude-code"})
    assert resp.json()["linked"] is True
    # cwd came from the pane (PaneInfo cwd="/tmp/x"), not the request.
    assert spawn_calls[0]["cwd"] == "/tmp/x"


@pytest.mark.asyncio
async def test_link_404_when_no_cwd_and_pane_not_live(tmp_path, monkeypatch):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    monkeypatch.setattr(peers_routes, "list_all_panes", lambda: [])  # pane not present
    async with async_client_for(harness.app) as client:
        resp = await client.post("/panes/%2551/link", json={"backend": "codex"})
    assert resp.status_code == 404
    assert resp.json()["detail"]["error"] == "pane_not_found"
    assert await harness.registry.get_peer_by_pane("%51") is None


@pytest.mark.asyncio
async def test_link_rolls_back_when_spawn_helper_raises(tmp_path, monkeypatch):
    # link_spawn_ws_hook catches its own spawn errors and returns False; assert
    # the route then rolls back (no ghost, metadata cleared) rather than letting
    # an exception escape past the rollback block.
    harness = make_daemon_app(tmp_path, [peers_routes.router])

    def boom(*_a, **_k):
        return False  # helper swallows the raise internally and reports failure

    monkeypatch.setattr(peers_routes, "link_spawn_ws_hook", boom)
    cleared: list[str] = []
    monkeypatch.setattr(
        peers_routes, "clear_pane_runtime_state", lambda pid: cleared.append(pid)
    )
    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%2552/link", json={"backend": "codex", "cwd": "/tmp/proj"}
        )
    body = resp.json()
    assert body["linked"] is False
    assert body["reason"] == "ws_hook_spawn_failed"
    assert cleared == ["%52"]  # rollback cleared the pane metadata
    assert await harness.registry.get_peer_by_pane("%52") is None


@pytest.mark.asyncio
async def test_link_does_not_persist_session_binding_or_cert(tmp_path, monkeypatch):
    # A linked orphan has no runtime session id, and a rolled-back link must
    # leave no durable binding/cert residue — so link registers with
    # persist_binding=False and never touches the binding store.
    class SpyBindingStore:
        def __init__(self):
            self.observations = 0
            self.certs = 0

        def upsert_observation(self, **_):
            self.observations += 1

        def mint_birth_certificate(self, **_):
            self.certs += 1
            return type("C", (), {"as_envelope": lambda self: {}})()

    spy = SpyBindingStore()
    harness = make_daemon_app(
        tmp_path, [peers_routes.router], state_overrides={"session_binding_store": spy}
    )
    monkeypatch.setattr(peers_routes, "link_spawn_ws_hook", lambda *a, **k: True)
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: True)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%2545/link", json={"backend": "claude-code", "cwd": "/tmp/proj"}
        )
    assert resp.json()["linked"] is True
    assert spy.observations == 0
    assert spy.certs == 0


@pytest.mark.asyncio
async def test_link_refuses_already_linked_pane(tmp_path):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    from repowire.protocol.peers import Peer

    await harness.registry.register_peer(
        Peer(peer_id="p9", display_name="bob", pane_id="%9", path="/tmp/b", machine="h")
    )
    async with async_client_for(harness.app) as client:
        resp = await client.post("/panes/%9/link", json={"backend": "codex"})
    assert resp.status_code == 409
    assert resp.json()["detail"]["error"] == "already_linked"
