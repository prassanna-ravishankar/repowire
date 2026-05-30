"""Tests for peer diagnostics: build_doctor_report, contradictions, and the route."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from repowire.daemon import diagnostics
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps
from repowire.daemon.diagnostics import (
    AGENT_PID_DEAD,
    HOOK_PEERID_MISMATCH,
    ONLINE_BUT_NO_WS,
    PANE_MISSING,
    STALE_PENDING_ASK,
    WS_PANE_MISMATCH,
    build_doctor_report,
)
from repowire.daemon.routes import asks, peers
from repowire.protocol.peers import Peer, PeerStatus

from .conftest import async_client_for, make_daemon_app

ROUTERS = (peers.router, asks.router)

LOCAL = "test-host.local"


class FakeTransport:
    """Minimal WebSocketTransport stand-in for diagnostics unit tests."""

    def __init__(self, connected: bool = False, pane_id: str | None = None) -> None:
        self._connected = connected
        self._pane_id = pane_id

    def is_connected(self, session_id: str) -> bool:  # noqa: ARG002
        return self._connected

    def get_connection_pane_id(self, session_id: str) -> str | None:  # noqa: ARG002
        return self._pane_id


def _peer(**overrides) -> Peer:
    peer = Peer(
        peer_id="repow-default-aaaa1111",
        display_name="alice",
        path="/tmp/alice",
        machine=LOCAL,
        circle="default",
        status=PeerStatus.ONLINE,
        pane_id="%1",
        agent_pid=4242,
    )
    return peer.model_copy(update=overrides) if overrides else peer


@pytest.fixture
def patched(monkeypatch):
    """Patch all local probes to benign defaults; tests override per-case."""
    monkeypatch.setattr(diagnostics, "probe_tmux_pane", lambda pane_id: _DummyEvidence(pane_id))
    monkeypatch.setattr(diagnostics, "read_pane_runtime_metadata", lambda pane_id: {})
    monkeypatch.setattr(diagnostics, "_agent_pid_alive", lambda pid: True)
    return monkeypatch


class _DummyEvidence:
    def __init__(self, pane_id: str) -> None:
        self.pane_id = pane_id
        self.tmux_session = "default:alice"
        self.current_path = "/tmp/alice"
        self.pane_pid = "9999"


async def _build(peer: Peer, transport: FakeTransport, ask_tracker=None):
    at = ask_tracker or AskTracker(ttl_hours=24.0)
    return await build_doctor_report(peer, transport, at, hostname=LOCAL)


class TestContradictions:
    async def test_clean_peer_no_contradictions(self, patched):
        peer = _peer()
        report = await _build(peer, FakeTransport(connected=True, pane_id="%1"))
        assert report.contradictions == []
        assert report.ws_connected is True
        assert report.tmux_pane_exists is True

    async def test_online_but_no_ws(self, patched):
        peer = _peer(status=PeerStatus.ONLINE)
        report = await _build(peer, FakeTransport(connected=False))
        codes = {c.code for c in report.contradictions}
        assert ONLINE_BUT_NO_WS in codes
        assert report.has_errors

    async def test_pane_missing(self, patched):
        patched.setattr(diagnostics, "probe_tmux_pane", lambda pane_id: None)
        peer = _peer()
        report = await _build(peer, FakeTransport(connected=True, pane_id="%1"))
        assert PANE_MISSING in {c.code for c in report.contradictions}
        assert report.tmux_pane_exists is False

    async def test_agent_pid_dead(self, patched):
        patched.setattr(diagnostics, "_agent_pid_alive", lambda pid: False)
        peer = _peer()
        report = await _build(peer, FakeTransport(connected=True, pane_id="%1"))
        assert AGENT_PID_DEAD in {c.code for c in report.contradictions}
        assert report.agent_pid_alive is False

    async def test_hook_peerid_mismatch(self, patched):
        patched.setattr(
            diagnostics,
            "read_pane_runtime_metadata",
            lambda pane_id: {"peer_id": "repow-default-bbbb2222", "display_name": "imposter"},
        )
        peer = _peer()
        report = await _build(peer, FakeTransport(connected=True, pane_id="%1"))
        assert HOOK_PEERID_MISMATCH in {c.code for c in report.contradictions}

    async def test_ws_pane_mismatch(self, patched):
        peer = _peer(pane_id="%1")
        report = await _build(peer, FakeTransport(connected=True, pane_id="%9"))
        codes = {c.code for c in report.contradictions}
        assert WS_PANE_MISMATCH in codes

    async def test_stale_pending_ask(self, patched):
        at = AskTracker(ttl_hours=24.0)
        peer = _peer()
        # Inject a stale open ask directly into the tracker.
        await at.register(
            correlation_id="ask-stale01",
            from_peer_id="repow-default-cccc3333",
            from_peer_name="bob",
            to_peer_id=peer.peer_id,
            to_peer_name=peer.display_name,
            text="old question",
        )
        old = datetime.now(timezone.utc) - timedelta(minutes=40)
        at._asks["ask-stale01"].created_at = old
        report = await _build(peer, FakeTransport(connected=True, pane_id="%1"), ask_tracker=at)
        assert STALE_PENDING_ASK in {c.code for c in report.contradictions}
        assert report.oldest_pending_cid == "ask-stale01"
        assert report.pending_inbound_count == 1

    async def test_remote_machine_degrades(self, patched):
        peer = _peer(machine="other-host.remote", status=PeerStatus.ONLINE)
        report = await _build(peer, FakeTransport(connected=False))
        # Local-only probes are unavailable...
        assert report.is_local_machine is False
        assert report.tmux_pane_exists is None
        assert report.hook_meta_available is False
        assert report.agent_pid_alive is None
        # ...and local-only contradictions are NOT asserted, even though offline+online.
        assert ONLINE_BUT_NO_WS not in {c.code for c in report.contradictions}


class TestDoctorRoute:
    @pytest.fixture
    async def env(self, tmp_path):
        harness = make_daemon_app(tmp_path, ROUTERS)
        async with async_client_for(harness.app) as c:
            yield c, harness
        cleanup_deps()

    async def _register(self, client, name: str) -> str:
        r = await client.post(
            "/peers",
            json={
                "name": name,
                "path": f"/tmp/{name}",
                "circle": "default",
                "backend": "claude-code",
            },
        )
        assert r.status_code == 200, r.text
        return r.json()["display_name"]

    async def test_200_known_peer(self, env):
        client, _ = env
        name = await self._register(client, "alice")
        r = await client.get(f"/peers/{name}/doctor")
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["display_name"] == name
        assert "contradictions" in body
        assert "ws_connected" in body

    async def test_404_unknown(self, env):
        client, _ = env
        r = await client.get("/peers/ghost-peer/doctor")
        assert r.status_code == 404

    async def test_surfaces_online_but_no_ws(self, env):
        client, harness = env
        name = await self._register(client, "alice")
        # Registered over HTTP only: no live ws connection. Force status ONLINE.
        peer = await harness.registry.get_peer(name)
        peer.status = PeerStatus.ONLINE
        r = await client.get(f"/peers/{name}/doctor")
        assert r.status_code == 200, r.text
        codes = {c["code"] for c in r.json()["contradictions"]}
        assert ONLINE_BUT_NO_WS in codes

    async def test_get_peer_exposes_inbound_health(self, env):
        client, harness = env
        name = await self._register(client, "alice")
        peer = await harness.registry.get_peer(name)
        # Seed a successful injection in the ledger so receipts are observed.
        harness.delivery_trace_store.record(
            trace_id="ask-x", kind="ask", stage="pane_injected", peer_id=peer.peer_id,
        )
        r = await client.get(f"/peers/{name}")
        assert r.status_code == 200, r.text
        body = r.json()
        assert "inbound_status" in body
        assert body["hook_supports_receipts"] is True  # observed delivery_ack
        assert body["last_successful_injection_at"] is not None
        assert body["ws_connected"] is False  # HTTP-only registration
