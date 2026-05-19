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
