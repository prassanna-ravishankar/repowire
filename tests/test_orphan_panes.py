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
    monkeypatch.setattr(peers_routes, "maybe_respawn", lambda *a, **k: True)
    # Transport reports the freshly-linked peer as connected.
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: True)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%7/link", json={"backend": "claude-code", "cwd": "/tmp/proj"}
        )
    assert resp.status_code == 200
    body = resp.json()
    assert body["linked"] is True
    assert body["transport_connected"] is True
    # The peer is in the roster bound to the pane.
    peer = await harness.registry.get_peer_by_pane("%7")
    assert peer is not None


@pytest.mark.asyncio
async def test_link_rolls_back_when_ws_never_connects(tmp_path, monkeypatch):
    harness = make_daemon_app(tmp_path, [peers_routes.router])
    monkeypatch.setattr(peers_routes, "maybe_respawn", lambda *a, **k: True)
    # WS never connects → fail-closed, no ghost left behind.
    monkeypatch.setattr(harness.transport, "is_connected", lambda _pid: False)
    monkeypatch.setattr(peers_routes, "_LINK_WS_WAIT_SECONDS", 0.05)

    async with async_client_for(harness.app) as client:
        resp = await client.post(
            "/panes/%8/link", json={"backend": "codex", "cwd": "/tmp/proj"}
        )
    assert resp.status_code == 200
    body = resp.json()
    assert body["linked"] is False
    assert body["transport_connected"] is False
    assert body["reason"] == "transport_unestablished"
    assert "repowire link --pane %8" in body["repair_hint"]
    # Rolled back: no ghost peer for the pane.
    assert await harness.registry.get_peer_by_pane("%8") is None


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
