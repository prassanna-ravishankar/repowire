"""Integration tests for the broker-side ACP client (phase-3).

End-to-end: register an ACP-routed peer in the daemon, post `/ask` with the
experiments flag on, and assert the prompt reaches an ACP subprocess and the
reply flows back through the existing ack pipeline.

The subprocess is a minimal echo agent at `tests/fixtures/acp_echo_agent.py`
that uses the official `acp` python SDK — same wire protocol codex-acp speaks.
"""

from __future__ import annotations

import asyncio
import sys
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.acp import AcpClientManager, decide_acp_route
from repowire.acp.broker import AcpRouteDecision
from repowire.acp.client import _assemble_agent_text
from repowire.acp.models import AcpPeerConfig
from repowire.config.models import Config
from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import asks, peers
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import AgentType, Peer, PeerStatus

STUB_PATH = Path(__file__).parent / "fixtures" / "acp_echo_agent.py"
CRASH_STUB_PATH = Path(__file__).parent / "fixtures" / "acp_crash_agent.py"
SLOW_STUB_PATH = Path(__file__).parent / "fixtures" / "acp_slow_agent.py"
PYTHON = sys.executable


@pytest.fixture
def acp_peer_config(tmp_path: Path) -> AcpPeerConfig:
    """Spawn config for the echo stub. Real codex-acp would slot in here."""
    return AcpPeerConfig(
        command=PYTHON,
        args=[str(STUB_PATH)],
        cwd=str(tmp_path),
    )


# ----- unit-level: route decision and text assembly -----


class TestRouteDecision:
    def test_flag_off_never_routes(self, acp_peer_config: AcpPeerConfig) -> None:
        peer = _peer_with_acp("p1", acp_peer_config)
        d = decide_acp_route(peer, flag_enabled=False)
        assert isinstance(d, AcpRouteDecision)
        assert d.route is False
        assert "off" in d.reason

    def test_flag_on_no_metadata_does_not_route(self) -> None:
        peer = _bare_peer("p2")
        d = decide_acp_route(peer, flag_enabled=True)
        assert d.route is False
        assert "no acp metadata" in d.reason

    def test_flag_on_with_metadata_routes(self, acp_peer_config: AcpPeerConfig) -> None:
        peer = _peer_with_acp("p3", acp_peer_config)
        d = decide_acp_route(peer, flag_enabled=True)
        assert d.route is True
        assert d.spec is not None
        assert d.spec.peer_id == "p3"
        assert d.spec.config.command == PYTHON

    def test_malformed_metadata_is_logged_and_skipped(self) -> None:
        peer = _bare_peer("p4")
        peer.metadata["acp"] = {"args": ["only-args-no-command"]}
        d = decide_acp_route(peer, flag_enabled=True)
        assert d.route is False
        assert "invalid acp metadata" in d.reason


def test_assemble_agent_text_concatenates_chunks() -> None:
    from acp.schema import AgentMessageChunk, TextContentBlock

    updates = [
        AgentMessageChunk(
            session_update="agent_message_chunk",
            content=TextContentBlock(type="text", text="hello "),
        ),
        AgentMessageChunk(
            session_update="agent_message_chunk",
            content=TextContentBlock(type="text", text="world"),
        ),
    ]
    assert _assemble_agent_text(updates) == "hello world"


# ----- integration: client + stub subprocess -----


@pytest.mark.asyncio
async def test_acp_client_prompt_roundtrip(acp_peer_config: AcpPeerConfig) -> None:
    """Direct ``AcpClient`` against the echo stub: prompt → end_turn + echoed text."""
    from repowire.acp import AcpClient

    async with AcpClient(acp_peer_config) as client:
        result = await client.prompt("hello world", timeout=15.0)
        assert result.stop_reason == "end_turn"
        assert result.text == "[echo] hello world"
        # session persists across asks
        result2 = await client.prompt("second turn", timeout=15.0)
        assert result2.text == "[echo] second turn"
        assert client.session_id is not None


@pytest.mark.asyncio
async def test_acp_manager_reuses_session_for_peer(acp_peer_config: AcpPeerConfig) -> None:
    """``AcpClientManager`` caches one client per peer_id; close tears it down."""
    from repowire.acp import AcpPeerSpec

    mgr = AcpClientManager()
    spec = AcpPeerSpec(peer_id="peer-xyz", config=acp_peer_config)
    try:
        r1 = await mgr.prompt(spec, "ping")
        r2 = await mgr.prompt(spec, "pong")
        assert r1.text == "[echo] ping"
        assert r2.text == "[echo] pong"
        c1 = await mgr.get_or_create(spec)
        c2 = await mgr.get_or_create(spec)
        assert c1 is c2  # same instance
    finally:
        await mgr.close()


@pytest.mark.asyncio
async def test_close_sends_session_close_before_teardown(
    acp_peer_config: AcpPeerConfig,
) -> None:
    # repowire-cju: close() should best-effort session/close before tearing down
    # stdio (lifecycle hygiene), distinct from cancel().
    from repowire.acp import AcpClient

    client = AcpClient(acp_peer_config)
    # Inject a fake live connection + session so close() exercises session/close
    # without needing the stub to implement it.
    closed: list[str] = []

    class _FakeConn:
        async def close_session(self, session_id: str, **_):
            closed.append(session_id)

    client._connection = _FakeConn()  # type: ignore[assignment]
    client._session_id = "sess-xyz"
    await client.close()
    assert closed == ["sess-xyz"], "close() must call session/close once"
    # Idempotent: a second close does not re-send.
    await client.close()
    assert closed == ["sess-xyz"]


@pytest.mark.asyncio
async def test_close_session_failure_does_not_break_teardown(
    acp_peer_config: AcpPeerConfig,
) -> None:
    # A session/close error (e.g. agent doesn't support it) must not block stdio
    # teardown — close stays best-effort.
    from repowire.acp import AcpClient

    client = AcpClient(acp_peer_config)

    class _BadConn:
        async def close_session(self, session_id: str, **_):
            raise RuntimeError("not supported")

    client._connection = _BadConn()  # type: ignore[assignment]
    client._session_id = "sess-abc"
    await client.close()  # must not raise
    assert client._closed is True


@pytest.mark.asyncio
async def test_manager_health_snapshot_exposes_acp_session_ids(
    acp_peer_config: AcpPeerConfig,
) -> None:
    # repowire-xig: ACP session ids are surfaced for diagnostics (NOT as a
    # runtime_session_id resume handle).
    from repowire.acp import AcpPeerSpec

    mgr = AcpClientManager()
    spec = AcpPeerSpec(peer_id="peer-diag", config=acp_peer_config)
    try:
        await mgr.prompt(spec, "ping")
        snap = mgr.health_snapshot()
        assert "peer-diag" in snap["sessions"]
        assert isinstance(snap["sessions"]["peer-diag"], str)
    finally:
        await mgr.close()


# ----- end-to-end: /ask via ACP delivers reply via notify -----


def _make_app(tmp_path: Path, *, flag: bool):
    cfg = Config()
    cfg.experiments.acp_broker_client = flag

    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=qt,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    registry._last_repair = time.monotonic() + 3600

    acp_manager = AcpClientManager()

    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
        acp_manager=acp_manager,
    )
    init_deps(cfg, registry, state)

    # The asker peer is a plain WS-routed peer; we intercept its notify path
    # because we don't run a real WS in the unit test. The captured payload IS
    # the assertion target for this test (does the ack reply round-trip?).
    sent_notifications: list[dict] = []

    async def _capture_notify(**kwargs):
        sent_notifications.append(kwargs)

    router.send_notification = AsyncMock(side_effect=_capture_notify)
    router.send_ask = AsyncMock()  # not used on the ACP path, but registered peers may use it

    app = FastAPI()
    app.include_router(peers.router)
    app.include_router(asks.router)
    return app, registry, at, acp_manager, sent_notifications


async def _register_acp_peer(
    client: AsyncClient, *, name: str, acp_config: AcpPeerConfig,
) -> str:
    """Register a peer in the daemon with `metadata.acp` set + a fake pane_id."""
    body = {
        "name": name,
        "path": acp_config.cwd or "/tmp",
        "circle": "default",
        "backend": AgentType.CODEX.value,
        "pane_id": f"%fake-{name}",
        "metadata": {"acp": acp_config.model_dump()},
    }
    r = await client.post("/peers", json=body)
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


async def _register_plain_peer(client: AsyncClient, *, name: str) -> str:
    r = await client.post("/peers", json={
        "name": name,
        "path": f"/tmp/{name}",
        "circle": "default",
        "backend": AgentType.CLAUDE_CODE.value,
        "pane_id": f"%fake-{name}",
    })
    assert r.status_code == 200, r.text
    return r.json()["display_name"]


@pytest.mark.asyncio
async def test_ask_routes_through_acp_when_flag_on(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """Phase-3 happy path: flag on + ACP-marked peer ⇒ prompt routed to subprocess,
    reply delivered to asker via the same notify pipeline ack uses today."""
    app, registry, at, manager, sent = _make_app(tmp_path, flag=True)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            asker = await _register_plain_peer(http, name="asker")
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            # Force both peers to ONLINE so notify doesn't trip the offline guard
            for p in await registry.get_all_peers():
                await registry.update_peer_status(p.peer_id, PeerStatus.ONLINE)

            r = await http.post("/ask", json={
                "from_peer": asker,
                "to_peer": answerer,
                "text": "what is the answer",
            })
            assert r.status_code == 200, r.text
            cid = r.json()["correlation_id"]

            # The /ask returns immediately; the ACP turn runs in a background task.
            # Wait for the ack notification to land.
            reply = await _wait_for_notification(sent, timeout=15.0)
            assert reply["text"].startswith(f"[ack #{cid} from @{answerer}]")
            assert "[echo] what is the answer" in reply["text"]

            # Ask thread is closed on success
            ask = await at.get(cid)
            assert ask is not None and ask.closed
    finally:
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_ask_falls_back_to_ws_when_flag_off(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """Flag off ⇒ ACP path is never taken, even if peer has acp metadata.

    The existing WS path is exercised; we just check that send_ask was called
    (i.e. the legacy delivery was attempted), and no ACP child was spawned.
    """
    app, registry, _at, manager, _sent = _make_app(tmp_path, flag=False)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            asker = await _register_plain_peer(http, name="asker")
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )
            for p in await registry.get_all_peers():
                await registry.update_peer_status(p.peer_id, PeerStatus.ONLINE)

            r = await http.post("/ask", json={
                "from_peer": asker,
                "to_peer": answerer,
                "text": "x",
            })
            assert r.status_code == 200, r.text

            # No ACP client should have been started
            assert manager._clients == {}  # type: ignore[attr-defined]
    finally:
        await manager.close()
        cleanup_deps()


# ----- public route-layer regression tests (repowire#206) -----


class _StubAcpManager:
    """In-process stand-in for ``AcpClientManager`` for route-layer tests.

    Records every prompt routed through it and returns canned results — no
    subprocess, no agent-client-protocol I/O. The route-layer tests only
    care that the /ask and /notify handlers reach an ACP manager at all;
    real subprocess plumbing is exercised by the existing end-to-end test.
    """

    def __init__(self) -> None:
        self.prompts: list[tuple[str, str]] = []  # (peer_id, text)
        self._clients: dict[str, object] = {}

    async def prompt(self, spec, text, timeout=180.0):  # noqa: ARG002
        from repowire.acp.client import AcpPromptResult
        self.prompts.append((spec.peer_id, text))
        return AcpPromptResult(stop_reason="end_turn", text=f"[stub] {text}")

    async def get_or_create(self, spec):  # noqa: ARG002
        return None  # unused in tests

    async def close(self) -> None:
        return None


def _make_public_app(tmp_path: Path, *, flag: bool, skip_lazy_repair: bool = False):
    """Mirror prod wiring: no peer-status fakery, no router-method mocks.

    This is the minimum set of components needed for /peers + /ask + /notify
    to behave like the real daemon. Notably:

      * peers register via real ``POST /peers`` (status starts at ONLINE),
      * no ``update_peer_status`` calls force ACP peers to look connected,
      * the WS transport is real but has zero connections (which is exactly
        the prod scenario for ACP peers),
      * ``lazy_repair`` is NOT short-circuited by default — the route handlers
        invoke it on entry just like prod. The audit on repowire#206 found
        that the original test suite skipped lazy_repair entirely, which
        hid the fact that ACP peers were being demoted/reaped for missing
        their (intentionally absent) WebSocket. Pass
        ``skip_lazy_repair=True`` only when you specifically want the
        prod-skip behavior.

    The asker-side reply notification will fail with TransportError (no WS
    on the asker), which is the right behavior — these tests assert on the
    /ask + /notify response only, not on the downstream ack delivery.
    """
    from repowire.daemon.routes import messages as messages_routes

    cfg = Config()
    cfg.experiments.acp_broker_client = flag

    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=qt,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    if skip_lazy_repair:
        registry._last_repair = time.monotonic() + 3600

    manager = _StubAcpManager()
    state = SimpleNamespace(
        config=cfg,
        transport=transport,
        query_tracker=qt,
        ask_tracker=at,
        message_router=router,
        peer_registry=registry,
        relay_mode=False,
        acp_manager=manager,
    )
    init_deps(cfg, registry, state)

    app = FastAPI()
    app.include_router(peers.router)
    app.include_router(asks.router)
    app.include_router(messages_routes.router)
    return app, registry, at, manager


@pytest.mark.asyncio
async def test_public_ask_route_does_not_503_for_acp_peer(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """repowire#206: ``POST /ask`` against an ACP peer must not 503.

    Mirrors the bug repro from the issue exactly: register an ACP peer via
    HTTP, register an asker via HTTP, do not force any peer ONLINE, then
    POST /ask. The route-layer ACP decision MUST fire before any
    WS-presence check, otherwise the brokered peer 503s the public surface
    (and MCP ``ask()`` along with it).
    """
    app, _registry, _at, manager = _make_public_app(tmp_path, flag=True)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            asker = await _register_plain_peer(http, name="asker")
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            r = await http.post("/ask", json={
                "from_peer": asker,
                "to_peer": answerer,
                "text": "ping",
            })
            assert r.status_code == 200, (
                f"/ask 503'd for ACP peer (regression of repowire#206): {r.text}"
            )
            assert r.json()["correlation_id"]
    finally:
        # Cancel any background ACP tasks before closing the manager so the
        # _run() coroutine doesn't see a closed manager mid-flight.
        await asyncio.sleep(0)
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_public_notify_route_does_not_503_for_acp_peer(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """repowire#206 sibling: ``POST /notify`` toward an ACP peer must not 503.

    Same route-layer issue as /ask — the WS-presence check inside
    ``peer_registry.notify`` fires before any ACP decision unless the route
    handler routes brokered targets to the ACP path explicitly. Asserts the
    HTTP surface returns 200 with ``status=sent`` (fire-and-forget mapping
    onto an ACP prompt).
    """
    app, _registry, _at, manager = _make_public_app(tmp_path, flag=True)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            asker = await _register_plain_peer(http, name="asker")
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            r = await http.post("/notify", json={
                "from_peer": asker,
                "to_peer": answerer,
                "text": "fyi",
            })
            assert r.status_code == 200, (
                f"/notify 503'd for ACP peer (regression of repowire#206): {r.text}"
            )
            body = r.json()
            assert body["status"] == "sent"
            assert body["delivery_state"] == "delivered"
            assert body["delivered"] is True
            assert body["queued"] is False
            # ACP notify is broker-accepted, not a runtime receipt (must-fix #3).
            assert body["reason"] == "broker_accepted"
            assert body["from_peer_name"] == asker
            assert body["to_peer_name"] == answerer
    finally:
        await asyncio.sleep(0)
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_broadcast_reaches_acp_peer_without_ws_session(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    # repowire-7bc: broadcast must reach ACP-routed peers (no WS session) via the
    # broker, reporting them in sent_to and prompting the ACP manager.
    app, _registry, _at, manager = _make_public_app(tmp_path, flag=True)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            sender = await _register_plain_peer(http, name="sender")
            acp_peer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            r = await http.post("/broadcast", json={
                "from_peer": sender,
                "text": "all hands",
            })
            assert r.status_code == 200, r.text
            assert acp_peer in r.json()["sent_to"]
            # the broker actually received the broadcast text
            await asyncio.sleep(0)
            assert any(text == "all hands" for _pid, text in manager.prompts)
    finally:
        await asyncio.sleep(0)
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_broadcast_excludes_named_acp_peer(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    # An explicitly-excluded ACP peer must not be prompted.
    app, _registry, _at, manager = _make_public_app(tmp_path, flag=True)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            sender = await _register_plain_peer(http, name="sender")
            acp_peer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            r = await http.post("/broadcast", json={
                "from_peer": sender,
                "text": "skip you",
                "exclude": [acp_peer],
            })
            assert r.status_code == 200, r.text
            assert acp_peer not in r.json()["sent_to"]
            await asyncio.sleep(0)
            assert manager.prompts == []
    finally:
        await asyncio.sleep(0)
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_broadcast_skips_offline_acp_peer(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    # Broadcast must not resurrect an OFFLINE ACP peer (codex review): match WS
    # broadcast semantics — only live peers are reached.
    app, registry, _at, manager = _make_public_app(
        tmp_path, flag=True, skip_lazy_repair=True,
    )
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            sender = await _register_plain_peer(http, name="sender")
            acp_peer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )
            # Mark the ACP peer OFFLINE (e.g. reaped/disconnected).
            for p in await registry.get_all_peers():
                if p.display_name == acp_peer:
                    p.status = PeerStatus.OFFLINE

            r = await http.post("/broadcast", json={
                "from_peer": sender,
                "text": "anyone home",
            })
            assert r.status_code == 200, r.text
            assert acp_peer not in r.json()["sent_to"]
            await asyncio.sleep(0)
            assert manager.prompts == []
    finally:
        await asyncio.sleep(0)
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_lazy_repair_does_not_demote_acp_peers_with_flag_on(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """repowire#206 audit follow-up: lazy_repair must not demote ACP peers.

    ``_demote_disconnected_peers`` marks any ONLINE peer without a WS
    connection as OFFLINE. ACP-brokered peers have no WS by design, so the
    original sweep silently took them offline ~30s after registration —
    and the reaper then evicted them entirely after the TTL. The fix
    exempts peers carrying ``metadata.acp`` while the
    ``experiments.acp_broker_client`` flag is on.
    """
    app, registry, _at, manager = _make_public_app(
        tmp_path, flag=True, skip_lazy_repair=True,
    )
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )
            peer = await registry.get_peer(answerer)
            assert peer is not None
            assert peer.status == PeerStatus.ONLINE

            demoted = await registry._demote_disconnected_peers()
            assert demoted == 0, (
                "ACP peer was demoted despite metadata.acp + flag on"
            )
            peer = await registry.get_peer(answerer)
            assert peer is not None and peer.status == PeerStatus.ONLINE
    finally:
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_lazy_repair_still_demotes_acp_peers_when_flag_off(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """Symmetric guard: the exemption is gated on the experiments flag.

    With the flag off, ACP routing isn't engaged, so metadata.acp is a dead
    annotation; brokered peers behave like any other paneless HTTP-registered
    peer and the ghost sweep should still demote them. Otherwise turning the
    flag off wouldn't be a clean way to disable the feature.
    """
    app, registry, _at, manager = _make_public_app(
        tmp_path, flag=False, skip_lazy_repair=True,
    )
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )
            demoted = await registry._demote_disconnected_peers()
            assert demoted == 1, (
                "flag-off ACP peer was unexpectedly exempted from demotion"
            )
            peer = await registry.get_peer(answerer)
            assert peer is not None and peer.status == PeerStatus.OFFLINE
    finally:
        await manager.close()
        cleanup_deps()


@pytest.mark.asyncio
async def test_acp_routing_enforces_circle_boundary(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """Review BLOCKING: ACP branch must not bypass circle enforcement.

    Pre-fix the ACP branch in /ask and /notify resolved the target peer
    only — the sender resolution + circle gate that the WS path runs
    inside ``peer_registry.notify`` / ``deliver_ask`` were silently
    skipped. A peer in circle A could ask/notify an ACP peer in circle B
    even without ``bypass_circle=True``. This test pins all three legs:

      * cross-circle without bypass → 403 on both /ask and /notify,
      * cross-circle WITH bypass → 200,
      * same-circle without bypass → 200.
    """
    app, _registry, _at, _manager = _make_public_app(
        tmp_path, flag=True, skip_lazy_repair=True,
    )
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            # Asker in circle "alpha", ACP target in circle "beta".
            asker_resp = await http.post("/peers", json={
                "name": "asker", "path": "/tmp/asker", "circle": "alpha",
                "backend": AgentType.CLAUDE_CODE.value, "pane_id": "%fake-asker",
            })
            assert asker_resp.status_code == 200
            asker = asker_resp.json()["display_name"]

            target_resp = await http.post("/peers", json={
                "name": "answerer", "path": "/tmp/answerer", "circle": "beta",
                "backend": AgentType.CODEX.value, "pane_id": "%fake-answerer",
                "metadata": {"acp": acp_peer_config.model_dump()},
            })
            assert target_resp.status_code == 200
            answerer = target_resp.json()["display_name"]

            # 1: cross-circle without bypass → 403 on /ask
            r = await http.post("/ask", json={
                "from_peer": asker, "to_peer": answerer,
                "circle": "beta", "text": "x",
            })
            assert r.status_code == 403, r.text
            assert "Circle boundary" in r.text

            # 2: cross-circle without bypass → 403 on /notify
            r = await http.post("/notify", json={
                "from_peer": asker, "to_peer": answerer,
                "circle": "beta", "text": "x",
            })
            assert r.status_code == 403, r.text
            assert "Circle boundary" in r.text

            # 3: cross-circle WITH bypass → 200 (both endpoints)
            r = await http.post("/ask", json={
                "from_peer": asker, "to_peer": answerer,
                "circle": "beta", "text": "x", "bypass_circle": True,
            })
            assert r.status_code == 200, r.text
            r = await http.post("/notify", json={
                "from_peer": asker, "to_peer": answerer,
                "circle": "beta", "text": "x", "bypass_circle": True,
            })
            assert r.status_code == 200, r.text

            # 4: same-circle without bypass → 200. Register a same-circle
            # ACP peer and confirm the gate isn't over-applied.
            same_circle_resp = await http.post("/peers", json={
                "name": "peer-in-alpha", "path": "/tmp/answerer2",
                "circle": "alpha", "backend": AgentType.CODEX.value,
                "pane_id": "%fake-answerer2",
                "metadata": {"acp": acp_peer_config.model_dump()},
            })
            assert same_circle_resp.status_code == 200
            same = same_circle_resp.json()["display_name"]
            r = await http.post("/ask", json={
                "from_peer": asker, "to_peer": same,
                "circle": "alpha", "text": "x",
            })
            assert r.status_code == 200, r.text
            r = await http.post("/notify", json={
                "from_peer": asker, "to_peer": same,
                "circle": "alpha", "text": "x",
            })
            assert r.status_code == 200, r.text
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_public_notify_flag_off_still_503s_acp_peer(
    tmp_path: Path, acp_peer_config: AcpPeerConfig,
) -> None:
    """Guard: with the experiments flag off, ACP peers fall through to WS.

    Locks in the contract that ACP routing is strictly opt-in. Without the
    flag, brokered peers behave like any other peer with no live WS — the
    route 503s, the manager stays empty.
    """
    app, _registry, _at, manager = _make_public_app(tmp_path, flag=False)
    try:
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as http:
            asker = await _register_plain_peer(http, name="asker")
            answerer = await _register_acp_peer(
                http, name="answerer", acp_config=acp_peer_config,
            )

            r = await http.post("/notify", json={
                "from_peer": asker,
                "to_peer": answerer,
                "text": "fyi",
            })
            assert r.status_code == 503, r.text
            assert manager._clients == {}  # type: ignore[attr-defined]
    finally:
        await manager.close()
        cleanup_deps()


# ----- helpers -----


async def _wait_for_notification(sink: list[dict], timeout: float) -> dict:
    deadline = time.monotonic() + timeout
    while not sink and time.monotonic() < deadline:
        await asyncio.sleep(0.05)
    assert sink, f"no notification received within {timeout}s"
    return sink[0]


def _bare_peer(peer_id: str) -> Peer:
    return Peer(
        peer_id=peer_id,
        display_name=peer_id,
        path="/tmp",
        machine="localhost",
    )


def _peer_with_acp(peer_id: str, cfg: AcpPeerConfig) -> Peer:
    p = _bare_peer(peer_id)
    p.metadata["acp"] = cfg.model_dump()
    return p


# ----- lifecycle regression tests (codex BLOCKING #1, #2, #3) -----


@pytest.mark.asyncio
async def test_manager_drops_client_when_subprocess_crashes(tmp_path: Path) -> None:
    """Codex BLOCKING #2: a crashed subprocess must not poison the manager cache.

    The crash-stub returns one normal response, then ``sys.exit(1)`` on the
    second prompt. Without the fix the manager would keep handing out the dead
    client forever; with the fix the second prompt raises ``AcpClientError``
    and the manager drops the cached entry so the third prompt respawns.
    """
    from repowire.acp import AcpClient, AcpClientError, AcpPeerSpec

    cfg = AcpPeerConfig(command=PYTHON, args=[str(CRASH_STUB_PATH)], cwd=str(tmp_path))
    mgr = AcpClientManager()
    spec = AcpPeerSpec(peer_id="crash-peer", config=cfg)
    try:
        # 1: ok
        r1 = await mgr.prompt(spec, "first")
        assert r1.text == "[ok] first"
        first_client = await mgr.get_or_create(spec)

        # 2: subprocess exits → AcpClientError, cached client evicted
        with pytest.raises(AcpClientError):
            await mgr.prompt(spec, "second")
        assert "crash-peer" not in mgr._clients  # type: ignore[attr-defined]
        assert first_client.crashed is True
        health = mgr.health_snapshot()
        assert health["last_error_peer_id"] == "crash-peer"
        assert "prompt failed" in health["last_error"]
        assert health["active_clients"] == 0

        # 3: a fresh client is spawned; we should get a normal echo again
        r3 = await mgr.prompt(spec, "third")
        assert r3.text == "[ok] third"
        assert mgr.health_snapshot()["last_error"] is None
        respawned = await mgr.get_or_create(spec)
        assert respawned is not first_client
        assert isinstance(respawned, AcpClient)
    finally:
        await mgr.close()


@pytest.mark.asyncio
async def test_prompt_timeout_closes_client_and_evicts_from_manager(tmp_path: Path) -> None:
    """Codex BLOCKING #3: a timed-out prompt must cancel + close, not leak.

    The slow-stub blocks forever in ``prompt``. With a short broker-side
    timeout we expect:
      * the prompt raises ``AcpClientError``
      * the client is marked crashed and closed
      * the manager has dropped the entry
      * a follow-up prompt respawns a fresh subprocess
    """
    from repowire.acp import AcpClientError, AcpPeerSpec

    cfg = AcpPeerConfig(command=PYTHON, args=[str(SLOW_STUB_PATH)], cwd=str(tmp_path))
    mgr = AcpClientManager()
    spec = AcpPeerSpec(peer_id="slow-peer", config=cfg)
    try:
        client = await mgr.get_or_create(spec)
        with pytest.raises(AcpClientError) as exc_info:
            await mgr.prompt(spec, "this will hang", timeout=1.0)
        assert "timed out" in str(exc_info.value)
        assert client.crashed is True
        assert "slow-peer" not in mgr._clients  # type: ignore[attr-defined]

        # A fresh prompt against a fresh stub (echo this time) should work,
        # proving the manager evicted cleanly. Using the echo stub keeps the
        # assertion strong without spending another full timeout.
        echo_cfg = AcpPeerConfig(command=PYTHON, args=[str(STUB_PATH)], cwd=str(tmp_path))
        r = await mgr.prompt(AcpPeerSpec(peer_id="slow-peer", config=echo_cfg), "alive")
        assert r.text == "[echo] alive"
    finally:
        await mgr.close()


@pytest.mark.asyncio
async def test_acp_complete_keeps_ask_open_when_asker_offline(tmp_path: Path) -> None:
    """Codex BLOCKING #1: TransportError from notify must leave ask open.

    Mirrors /ack's fail-loud contract: a successful ACP turn whose ack
    notification can't be delivered (asker has no live WS) is NOT silently
    dropped. The ask stays open so the reply can be redelivered on reconnect.

    To reach the TransportError branch the asker peer must be registered (so
    registry lookup succeeds) but its underlying transport.send must raise.
    """
    from repowire.daemon.routes.asks import _acp_complete
    from repowire.daemon.websocket_transport import TransportError

    cfg = Config()
    cfg.experiments.acp_broker_client = True
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg,
        message_router=router,
        query_tracker=qt,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    # Register both peers so registry lookups succeed; we want the TransportError
    # to come from the wire layer, not from "unknown peer".
    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
        status=PeerStatus.ONLINE,
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
        status=PeerStatus.ONLINE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    # Make the wire-level send raise TransportError (asker WS dead)
    async def _boom(**_):
        raise TransportError("no live ws")

    router.send_notification = AsyncMock(side_effect=_boom)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="hi",
    )

    await _acp_complete(
        correlation_id=cid,
        reply="here is your answer",
        error=None,
        ask_tracker=at,
        peer_registry=registry,
    )

    # Ask MUST still be open after a TransportError so the reply can retry,
    # AND the assembled reply must be stashed on the ask for redelivery.
    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is False, "ask was closed despite asker being offline"
    assert ask.pending_reply is not None
    assert "here is your answer" in ask.pending_reply
    assert ask.pending_reply.startswith(f"[ack #{cid} from @{answerer.display_name}]")


@pytest.mark.asyncio
async def test_acp_complete_drops_error_frame_when_asker_offline(tmp_path: Path) -> None:
    """Error-frame replies are not worth redelivering — close on TransportError.

    The framed ``ACP error: ...`` body is ephemeral diagnostic text; the asker
    can't act on a stale failure they didn't observe in real time. Surfacing
    it on reconnect would just confuse the agent's later context. Codex's
    review correctly flagged the success-vs-error asymmetry as worth making
    explicit.
    """
    from repowire.daemon.routes.asks import _acp_complete
    from repowire.daemon.websocket_transport import TransportError

    cfg = Config()
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
        status=PeerStatus.ONLINE,
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
        status=PeerStatus.ONLINE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    async def _boom(**_):
        raise TransportError("no live ws")

    router.send_notification = AsyncMock(side_effect=_boom)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="hi",
    )
    await _acp_complete(
        correlation_id=cid, reply=None, error="subprocess died",
        ask_tracker=at, peer_registry=registry,
    )
    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True
    assert ask.close_reason == "send_failed"
    assert ask.pending_reply is None


@pytest.mark.asyncio
async def test_acp_complete_closes_ask_on_success(tmp_path: Path) -> None:
    """Happy path: successful notify closes the ask with ack_with_msg.

    The earlier revision of this test did NOT register the asker/answerer in
    the registry, so ``peer_registry.notify`` actually hit the ValueError
    branch and closed the ask as 'unknown peer' — not as success. Codex's
    re-review flagged that the test name was misleading because of this.
    """
    from repowire.daemon.routes.asks import _acp_complete

    cfg = Config()
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600
    router.send_notification = AsyncMock()

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
        status=PeerStatus.ONLINE,
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
        status=PeerStatus.ONLINE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="hi",
    )
    await _acp_complete(
        correlation_id=cid, reply="answer", error=None,
        ask_tracker=at, peer_registry=registry,
    )
    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True
    assert ask.close_reason == "ack_with_msg"
    # send_notification was actually invoked with our framed reply
    router.send_notification.assert_called_once()
    kwargs = router.send_notification.call_args.kwargs
    assert "answer" in kwargs["text"]
    assert kwargs["text"].startswith(f"[ack #{cid}")


@pytest.mark.asyncio
async def test_pending_reply_redelivered_on_asker_reconnect(tmp_path: Path) -> None:
    """End-to-end of codex BLOCKING #1's durable fix.

    1. ACP-routed ask completes successfully, but asker is offline
       (TransportError on notify) → reply stashed on the ask.
    2. Asker transitions OFFLINE → ONLINE via update_peer_status.
    3. peer_registry kicks off _redeliver_pending_replies; on this second
       notify attempt the transport succeeds → ask closed, stash cleared.

    This is the contract codex held us to: the answer is durable across the
    reconnect, not silently lost.
    """
    cfg = Config()
    cfg.experiments.acp_broker_client = True
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)
    # register_peer always lands at ONLINE — drop the asker to OFFLINE so the
    # next transition is a real OFFLINE → ONLINE bump.
    await registry.update_peer_status(asker.peer_id, PeerStatus.OFFLINE)

    from repowire.daemon.routes.asks import _acp_complete
    from repowire.daemon.websocket_transport import TransportError

    delivery_attempts: list[dict] = []
    fail_first = {"count": 0}

    async def _maybe_fail(**kwargs):
        delivery_attempts.append(kwargs)
        fail_first["count"] += 1
        if fail_first["count"] == 1:
            raise TransportError("asker not connected yet")

    router.send_notification = AsyncMock(side_effect=_maybe_fail)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="what's the time",
    )

    # Step 1: ACP completes; first notify attempt fails → stash
    await _acp_complete(
        correlation_id=cid, reply="42 minutes past noon", error=None,
        ask_tracker=at, peer_registry=registry,
    )
    ask = await at.get(cid)
    assert ask is not None and not ask.closed
    assert ask.pending_reply is not None

    # Step 2: asker comes back online. update_peer_status schedules a redeliver
    # background task; await it directly so we don't rely on event-loop timing.
    await registry.update_peer_status(asker.peer_id, PeerStatus.ONLINE)
    # Give the spawned redeliver task one tick to run.
    await asyncio.sleep(0.05)

    # Step 3: second notify attempt succeeded → ask closed, stash drained
    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True, "ask should be closed after successful redelivery"
    assert ask.close_reason == "ack_with_msg"
    assert len(delivery_attempts) == 2, f"expected 2 notify attempts, got {len(delivery_attempts)}"
    assert "42 minutes past noon" in delivery_attempts[1]["text"]


@pytest.mark.asyncio
async def test_reconnect_redelivery_is_skipped_when_flag_off(tmp_path: Path) -> None:
    """Codex re-review BLOCKER: flag-off ⇒ zero new behaviour on reconnect.

    With ``experiments.acp_broker_client`` off, an OFFLINE→ONLINE transition
    must NOT scan the ask tracker and must NOT call notify on the asker's
    behalf — even if a pending_reply somehow ended up stashed. The phase-3
    contract is strict: flag off means the WS/MCP path runs unchanged.

    We seed a stashed reply directly into the tracker (bypassing the
    flag-gated _acp_complete write path) and assert that the reconnect
    hook leaves it untouched. With the flag on, the same scenario would
    drain and deliver — proven by the redelivery test above.
    """
    cfg = Config()
    cfg.experiments.acp_broker_client = False  # explicitly off

    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)
    await registry.update_peer_status(asker.peer_id, PeerStatus.OFFLINE)

    notify_calls: list[dict] = []

    async def _record(**kwargs):
        notify_calls.append(kwargs)

    router.send_notification = AsyncMock(side_effect=_record)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="hi",
    )
    # Seed the stash directly (the flag-gated _acp_complete is the only normal
    # source, but the assertion is "if a stash exists, flag-off must ignore it")
    stashed = await at.set_pending_reply(cid, f"[ack #{cid} from @answerer] stale")
    assert stashed is True

    await registry.update_peer_status(asker.peer_id, PeerStatus.ONLINE)
    await asyncio.sleep(0.05)

    # With flag off: no redeliver task scheduled → no notify call,
    # ask still open, pending_reply still on the ask.
    assert notify_calls == [], "flag-off path called notify on reconnect"
    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is False
    assert ask.pending_reply is not None


@pytest.mark.asyncio
async def test_pending_reply_cleared_after_successful_redelivery(tmp_path: Path) -> None:
    """NIT from codex: don't retain reply text past its useful life.

    After a redelivery succeeds the ask is closed, but the Ask object would
    still carry the stashed text without an explicit clear. Verify the
    redelivery path nulls pending_reply.
    """
    cfg = Config()
    cfg.experiments.acp_broker_client = True
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)
    await registry.update_peer_status(asker.peer_id, PeerStatus.OFFLINE)

    router.send_notification = AsyncMock()  # succeeds

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="hi",
    )
    await at.set_pending_reply(cid, f"[ack #{cid} from @answerer] body")

    await registry.update_peer_status(asker.peer_id, PeerStatus.ONLINE)
    await asyncio.sleep(0.05)

    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True
    assert ask.pending_reply is None


@pytest.mark.asyncio
async def test_pending_reply_kept_when_redelivery_still_fails(tmp_path: Path) -> None:
    """Redelivery is best-effort: if it still fails, the stash stays put.

    Future reconnects of the same asker will trigger another redelivery
    attempt. We don't drop the reply just because one attempt was wasted.
    """
    cfg = Config()
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600

    asker = Peer(
        peer_id="asker-id", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%asker", circle="default",
    )
    answerer = Peer(
        peer_id="answerer-id", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%answerer", circle="default",
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)
    # register_peer always lands at ONLINE — drop the asker to OFFLINE so the
    # next transition is a real OFFLINE → ONLINE bump.
    await registry.update_peer_status(asker.peer_id, PeerStatus.OFFLINE)

    from repowire.daemon.routes.asks import _acp_complete
    from repowire.daemon.websocket_transport import TransportError

    async def _always_boom(**_):
        raise TransportError("still offline")
    router.send_notification = AsyncMock(side_effect=_always_boom)

    cid = await at.register(
        from_peer_id=asker.peer_id, from_peer_name=asker.display_name,
        to_peer_id=answerer.peer_id, to_peer_name=answerer.display_name,
        text="ping",
    )
    await _acp_complete(
        correlation_id=cid, reply="pong", error=None,
        ask_tracker=at, peer_registry=registry,
    )
    await registry.update_peer_status(asker.peer_id, PeerStatus.ONLINE)
    await asyncio.sleep(0.05)

    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is False, "ask kept open after failed redelivery"
    assert ask.pending_reply is not None, "stash retained for next reconnect"


def test_make_recorder_raises_clean_error_when_acp_sdk_missing(monkeypatch) -> None:
    """When the optional `agent-client-protocol` runtime isn't installed,
    AcpClient construction must fail with an AcpClientError carrying an
    install hint — not a bare ModuleNotFoundError leaked from the import.

    Regression: smoke test of v0.13.27 deployment surfaced bare
    `ModuleNotFoundError: No module named 'acp'` in daemon logs, blocking
    diagnosis until reading source. The hint shortcircuits that.
    """
    import builtins
    import sys

    from repowire.acp import client as acp_client_mod

    # Drop any cached acp.* modules so the test sees a real ImportError path.
    for name in list(sys.modules):
        if name == "acp" or name.startswith("acp."):
            monkeypatch.delitem(sys.modules, name, raising=False)

    real_import = builtins.__import__

    def fake_import(name, globals=None, locals=None, fromlist=(), level=0):
        if name == "acp" or name.startswith("acp."):
            raise ModuleNotFoundError(f"No module named '{name}'")
        return real_import(name, globals, locals, fromlist, level)

    monkeypatch.setattr(builtins, "__import__", fake_import)

    with pytest.raises(acp_client_mod.AcpClientError) as exc_info:
        acp_client_mod._make_recorder()

    msg = str(exc_info.value)
    assert "agent-client-protocol" in msg
    assert "experiments.acp_broker_client" in msg


# ----------------------------------------------------------------------
# #207 — identity-tuple rebind, pointer-only loss events, TTL ordering
# ----------------------------------------------------------------------

from datetime import datetime, timedelta, timezone  # noqa: E402


def _make_registry_with_at(tmp_path: Path, *, flag: bool = True) -> tuple:
    cfg = Config()
    cfg.experiments.acp_broker_client = flag
    transport = WebSocketTransport()
    qt = QueryTracker()
    at = AskTracker(ttl_hours=24.0)
    router = MessageRouter(transport=transport, query_tracker=qt)
    registry = PeerRegistry(
        config=cfg, message_router=router, query_tracker=qt,
        transport=transport, persistence_path=tmp_path / "sessions.json",
        ask_tracker=at,
    )
    registry._events_path = tmp_path / "events.json"
    registry._last_repair = time.monotonic() + 3600  # block auto lazy_repair
    return cfg, registry, router, at


@pytest.mark.asyncio
async def test_pending_reply_rebinds_on_new_peer_id_after_clean_takeover(
    tmp_path: Path,
) -> None:
    """#207 path A: clean-takeover prune leaves a stash; rebind via identity tuple.

    Drives the bug through the real SessionStart entry point —
    ``allocate_and_register`` — so we prove pass-2 redelivery is wired into
    the path users actually hit (fresh ONLINE allocation), not just
    update_peer_status's OFFLINE→ONLINE bump.

    1. Asker A registered via allocate_and_register, goes OFFLINE with a
       stashed reply (full identity captured).
    2. Second allocate_and_register call with the same path/backend/circle
       — _build_display_name prunes the OFFLINE A and a fresh peer_id is
       allocated, ONLINE from birth.
    3. allocate_and_register's post-lock scheduler kicks
       _redeliver_pending_replies on the new id; pass-2 identity match
       finds the orphan and delivers. Ask closes; no pending_reply_lost
       event.
    """
    from repowire.daemon.routes.asks import _acp_complete

    cfg, registry, router, at = _make_registry_with_at(tmp_path)

    # Real path: allocate_and_register for both asker and answerer.
    asker_a_id, asker_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%a",
        machine="localhost",
    )
    answerer_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path / "answerer"),
        pane_id="%ans",
        machine="localhost",
    )
    await registry.update_peer_status(asker_a_id, PeerStatus.OFFLINE)

    # Stash a reply via the real _acp_complete path so identity is captured.
    fail_first = {"n": 0}

    async def _maybe_fail(**kwargs):
        fail_first["n"] += 1
        if fail_first["n"] == 1:
            from repowire.daemon.websocket_transport import TransportError
            raise TransportError("asker offline")
    router.send_notification = AsyncMock(side_effect=_maybe_fail)

    cid = await at.register(
        from_peer_id=asker_a_id, from_peer_name=asker_name,
        to_peer_id=answerer_id, to_peer_name="answerer", text="q",
    )
    await _acp_complete(
        correlation_id=cid, reply="42", error=None,
        ask_tracker=at, peer_registry=registry,
    )
    ask = await at.get(cid)
    assert ask is not None and ask.pending_reply is not None
    assert ask.asker_identity is not None, "identity should have been captured"

    # Real clean-takeover: a second allocate_and_register for the same
    # name/circle/backend with the OFFLINE asker-A still present prunes A
    # (via _build_display_name) and issues a brand new peer_id.
    asker_b_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%a",
        machine="localhost",
    )
    assert asker_b_id != asker_a_id, "takeover should yield a fresh peer_id"
    # Give the post-lock redelivery task a tick to run.
    await asyncio.sleep(0.1)

    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True, "rebind should have delivered + closed the ask"
    assert ask.close_reason == "ack_with_msg"
    assert ask.pending_reply is None, "stash should be cleared after delivery"
    assert ask.from_peer_id == asker_b_id, "from_peer_id should be rewritten on rebind"
    # No loss event emitted
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert events == []


@pytest.mark.asyncio
async def test_pending_reply_rebind_refused_when_identity_incomplete(
    tmp_path: Path,
) -> None:
    """If the stash was captured without identity (e.g. asker had machine=unknown
    at stash time), pass-2 cannot rebind — reply stays stashed."""
    cfg, registry, router, at = _make_registry_with_at(tmp_path)
    notify_calls: list[dict] = []

    async def _record(**kwargs):
        notify_calls.append(kwargs)
    router.send_notification = AsyncMock(side_effect=_record)

    asker_a = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker_a)
    await registry.register_peer(answerer)

    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    # Stash without identity (simulating machine=unknown gate refusal at stash time)
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42", identity=None)

    # Prune A; register B with same tuple
    async with registry._lock:
        del registry._peers["asker-A"]
        registry._mappings.pop("asker-A", None)
    asker_b = Peer(
        peer_id="asker-B", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker_b)
    await registry.update_peer_status("asker-B", PeerStatus.OFFLINE)
    await registry.update_peer_status("asker-B", PeerStatus.ONLINE)
    await asyncio.sleep(0.1)

    ask = await at.get(cid)
    assert ask is not None and ask.closed is False
    assert ask.pending_reply is not None
    assert notify_calls == [], "no rebind should have fired without identity"


@pytest.mark.asyncio
async def test_pending_reply_rebind_refused_on_ambiguous_live_match(
    tmp_path: Path,
) -> None:
    """True full-tuple collision: two live peers share every identity field.

    Reachable only via the lower-level register_peer path because
    allocate_and_register's name-builder suffixes collisions. The registry
    uniqueness gate must refuse rebind rather than misroute.
    """
    cfg, registry, router, at = _make_registry_with_at(tmp_path)
    notify_calls: list[dict] = []
    async def _record(**kwargs):
        notify_calls.append(kwargs)
    router.send_notification = AsyncMock(side_effect=_record)

    asker_a = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker_a)
    await registry.register_peer(answerer)
    await registry.update_peer_status("asker-A", PeerStatus.OFFLINE)

    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    from repowire.daemon.ask_tracker import AskerIdentity
    from repowire.daemon.peer_registry import normalize_identity_path
    norm = normalize_identity_path(str(tmp_path))
    ident = AskerIdentity(
        display_name="asker", circle="default", backend="claude-code",
        path=norm, machine="localhost",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42", identity=ident)

    # Construct the pathological state: two live peers, same full tuple,
    # different peer_ids. (Use direct dict insertion to bypass the
    # de-duplication path inside register_peer.)
    async with registry._lock:
        del registry._peers["asker-A"]
        registry._mappings.pop("asker-A", None)
    asker_b = Peer(
        peer_id="asker-B", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    asker_c = Peer(
        peer_id="asker-C", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id=None, circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    async with registry._lock:
        registry._peers["asker-B"] = asker_b
        registry._peers["asker-C"] = asker_c

    await registry.update_peer_status("asker-B", PeerStatus.OFFLINE)
    await registry.update_peer_status("asker-B", PeerStatus.ONLINE)
    await asyncio.sleep(0.1)

    ask = await at.get(cid)
    assert ask is not None and ask.closed is False
    assert ask.pending_reply is not None
    assert notify_calls == [], "ambiguous tuple must refuse rebind"
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert events == [], "no loss event yet — still recoverable on next sweep"


@pytest.mark.asyncio
async def test_pending_reply_lost_event_on_reap(tmp_path: Path) -> None:
    """Reap-time pointer-only loss event with answerer fields."""
    cfg, registry, router, at = _make_registry_with_at(tmp_path)
    # Configure short reap TTL so we can backdate cheaply.
    cfg.daemon.peer_reap_ttl_seconds = 60

    asker = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    from repowire.daemon.ask_tracker import AskerIdentity
    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    ident = AskerIdentity(
        display_name="asker", circle="default", backend="claude-code",
        path=str(tmp_path), machine="localhost",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] secret answer", identity=ident)

    # Backdate asker to past reap TTL and mark OFFLINE
    async with registry._lock:
        live = registry._peers["asker-A"]
        live.status = PeerStatus.OFFLINE
        live.last_seen = datetime.now(timezone.utc) - timedelta(seconds=120)

    # Allow lazy_repair to run
    registry._last_repair = 0.0
    await registry.lazy_repair()

    # Ask should be gone (forget_peer dropped it after the snapshot)
    assert await at.get(cid) is None
    # And we should have one pointer-only loss event
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert len(events) == 1
    ev = events[0]
    assert ev["correlation_id"] == cid
    assert ev["answerer_peer_id"] == "answerer"
    assert ev["answerer_name"] == "answerer"
    # asker_name is always present (from Ask.from_peer_name)
    assert ev["asker_name"] == "asker"
    assert ev["asker_display_name"] == "asker"
    assert ev["asker_circle"] == "default"
    assert ev["asker_backend"] == "claude-code"
    assert ev["asker_peer_id"] == "asker-A"
    assert ev["reason"] == "offline_ttl_reap"
    # Pointer-only: no reply text, no asker path, no asker machine
    assert "reply" not in ev and "text" not in ev
    assert "path" not in ev and "machine" not in ev


@pytest.mark.asyncio
async def test_pending_reply_lost_event_on_ttl_eviction(tmp_path: Path) -> None:
    """24h TTL eviction emits a loss event via lazy_repair's single-owner path."""
    cfg, registry, router, at = _make_registry_with_at(tmp_path)

    asker = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42")

    # Backdate created_at past 24h TTL
    ask = await at.get(cid)
    ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)

    registry._last_repair = 0.0
    await registry.lazy_repair()

    assert await at.get(cid) is None
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert len(events) == 1
    assert events[0]["reason"] == "ttl_evicted"
    assert events[0]["correlation_id"] == cid
    assert events[0]["answerer_peer_id"] == "answerer"


@pytest.mark.asyncio
async def test_stop_hook_poll_does_not_silently_evict_stashed_ask(
    tmp_path: Path,
) -> None:
    """The Stop-hook-driven pending_for_peer path must NOT drop stashed-expired
    asks before the registry's lazy_repair gets a chance to emit the loss event.
    """
    cfg, registry, router, at = _make_registry_with_at(tmp_path)

    asker = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)
    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42")
    ask = await at.get(cid)
    ask.created_at = datetime.now(timezone.utc) - timedelta(hours=25)

    # Force _maybe_evict_expired to run on the next pending_for_peer call.
    at._last_eviction = 0.0
    _ = await at.pending_for_peer("answerer")

    # Ask is still present; no loss event yet.
    assert await at.get(cid) is not None
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert events == []

    # Registry sweep is the single owner — now it should emit + delete.
    registry._last_repair = 0.0
    await registry.lazy_repair()
    assert await at.get(cid) is None
    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert len(events) == 1
    assert events[0]["reason"] == "ttl_evicted"


@pytest.mark.asyncio
async def test_path_normalization_symmetry(tmp_path: Path) -> None:
    """Stash with raw path; live peer normalized variant; rebind succeeds."""
    cfg, registry, router, at = _make_registry_with_at(tmp_path)
    notify_calls: list[dict] = []
    async def _record(**kwargs):
        notify_calls.append(kwargs)
    router.send_notification = AsyncMock(side_effect=_record)

    from repowire.daemon.ask_tracker import AskerIdentity
    from repowire.daemon.peer_registry import normalize_identity_path

    raw = str(tmp_path) + "/./"  # un-normalized
    norm = normalize_identity_path(raw)

    asker_a = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker_a)
    await registry.register_peer(answerer)
    await registry.update_peer_status("asker-A", PeerStatus.OFFLINE)

    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    ident = AskerIdentity(
        display_name="asker", circle="default", backend="claude-code",
        path=norm, machine="localhost",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42", identity=ident)

    # Prune A, register B with the un-normalized variant. Registry's pass-2
    # normalizes both sides, so the rebind tuple should match.
    async with registry._lock:
        del registry._peers["asker-A"]
        registry._mappings.pop("asker-A", None)
    asker_b = Peer(
        peer_id="asker-B", display_name="asker", path=raw,
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker_b)
    await registry.update_peer_status("asker-B", PeerStatus.OFFLINE)
    await registry.update_peer_status("asker-B", PeerStatus.ONLINE)
    await asyncio.sleep(0.1)

    ask = await at.get(cid)
    assert ask is not None and ask.closed is True
    assert ask.pending_reply is None


@pytest.mark.asyncio
async def test_same_id_reconnect_via_allocate_and_register_redelivers(
    tmp_path: Path,
) -> None:
    """Same-peer_id reconnect through allocate_and_register also triggers
    redelivery — not just update_peer_status's OFFLINE→ONLINE bump.

    HTTP /peers pre-registration followed by a WebSocket reconnect calls
    allocate_and_register with the original peer_id; the peer takes over
    in place (no new peer_id). Pass-1 (same-id) redelivery should fire.
    """
    from repowire.daemon.routes.asks import _acp_complete

    cfg, registry, router, at = _make_registry_with_at(tmp_path)

    asker_id, asker_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%a",
        machine="localhost",
    )
    answerer_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path / "answerer"),
        pane_id="%ans",
        machine="localhost",
    )
    await registry.update_peer_status(asker_id, PeerStatus.OFFLINE)

    fail_first = {"n": 0}

    async def _maybe_fail(**kwargs):
        fail_first["n"] += 1
        if fail_first["n"] == 1:
            from repowire.daemon.websocket_transport import TransportError
            raise TransportError("asker offline")
    router.send_notification = AsyncMock(side_effect=_maybe_fail)

    cid = await at.register(
        from_peer_id=asker_id, from_peer_name=asker_name,
        to_peer_id=answerer_id, to_peer_name="answerer", text="q",
    )
    await _acp_complete(
        correlation_id=cid, reply="42", error=None,
        ask_tracker=at, peer_registry=registry,
    )

    # Same-id reconnect through allocate_and_register: peer_id carried in.
    same_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%a",
        machine="localhost",
        peer_id=asker_id,
    )
    assert same_id == asker_id, "reconnect must keep the original peer_id"
    await asyncio.sleep(0.1)

    ask = await at.get(cid)
    assert ask is not None
    assert ask.closed is True, "pass-1 redelivery should have closed the ask"
    assert ask.pending_reply is None
    assert ask.from_peer_id == asker_id  # unchanged, no rebind needed


@pytest.mark.asyncio
async def test_pending_reply_lost_includes_asker_name_without_identity(
    tmp_path: Path,
) -> None:
    """Blocker #4: asker_name must be populated from Ask.from_peer_name
    even when asker_identity is None (e.g. stash captured without identity
    because machine=='unknown' at stash time)."""
    cfg, registry, _, at = _make_registry_with_at(tmp_path)
    cfg.daemon.peer_reap_ttl_seconds = 60

    asker = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    answerer = Peer(
        peer_id="answerer", display_name="answerer", path=str(tmp_path),
        machine="localhost", pane_id="%ans", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker)
    await registry.register_peer(answerer)

    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker-display",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    # No identity captured (simulates pre-strictness-gate stash)
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42", identity=None)

    async with registry._lock:
        live = registry._peers["asker-A"]
        live.status = PeerStatus.OFFLINE
        live.last_seen = datetime.now(timezone.utc) - timedelta(seconds=120)

    registry._last_repair = 0.0
    await registry.lazy_repair()

    events = [e for e in registry.get_events() if e["type"] == "pending_reply_lost"]
    assert len(events) == 1
    ev = events[0]
    assert ev["asker_name"] == "asker-display", "asker_name must come from Ask.from_peer_name"
    assert ev["asker_display_name"] is None  # identity wasn't captured
    assert ev["asker_circle"] is None
    assert ev["asker_backend"] is None
    # Pointer-only invariants still hold
    assert "reply" not in ev and "text" not in ev
    assert "path" not in ev and "machine" not in ev


@pytest.mark.asyncio
async def test_pending_reply_lost_emitted_before_destruction(
    tmp_path: Path,
) -> None:
    """Blocker #3: snapshot → emit → forget ordering. Capture the event
    payload at emit time AND confirm the ask is still present at that
    moment, then absent after lazy_repair completes."""
    cfg, registry, _, at = _make_registry_with_at(tmp_path)
    cfg.daemon.peer_reap_ttl_seconds = 60

    asker = Peer(
        peer_id="asker-A", display_name="asker", path=str(tmp_path),
        machine="localhost", pane_id="%a", circle="default",
        backend=AgentType.CLAUDE_CODE,
    )
    await registry.register_peer(asker)
    cid = await at.register(
        from_peer_id="asker-A", from_peer_name="asker",
        to_peer_id="answerer", to_peer_name="answerer", text="q",
    )
    await at.set_pending_reply(cid, "[ack #x from @answerer] 42")

    async with registry._lock:
        live = registry._peers["asker-A"]
        live.status = PeerStatus.OFFLINE
        live.last_seen = datetime.now(timezone.utc) - timedelta(seconds=120)

    # Hook add_event so we can check the ask still exists at emission time.
    seen_at_emit: dict = {}
    original_add_event = registry.add_event

    def _spy_add_event(name, payload):
        if name == "pending_reply_lost":
            # If ordering is wrong (delete first), the tracker would already
            # have removed the ask by now.
            seen_at_emit["ask_exists"] = cid in at._asks
        return original_add_event(name, payload)
    registry.add_event = _spy_add_event  # type: ignore[assignment]

    registry._last_repair = 0.0
    await registry.lazy_repair()

    assert seen_at_emit.get("ask_exists") is True, (
        "event must fire BEFORE the ask is forgotten — observed deleted at emit"
    )
    assert await at.get(cid) is None, "ask must be gone after lazy_repair completes"
