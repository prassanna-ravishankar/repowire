from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from click.testing import CliRunner
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient

from repowire.cli import main
from repowire.config.models import AgentType, Config
from repowire.daemon.deps import cleanup_deps, init_deps
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry, RoleClaimConflictError
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.routes import peers
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import PeerRole, PeerStatus


def _make_registry(tmp_path: Path) -> PeerRegistry:
    cfg = Config()
    cfg.daemon.heartbeat_interval = 30
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
    registry._last_repair = 10**9
    return registry


async def _register(
    registry: PeerRegistry,
    *,
    path: str,
    circle: str = "ops",
    role: PeerRole = PeerRole.AGENT,
) -> str:
    peer_id, _ = await registry.allocate_and_register(
        circle=circle,
        backend=AgentType.CODEX,
        path=path,
        role=role,
        machine="m",
    )
    return peer_id


@pytest.mark.asyncio
async def test_claim_orchestrator_demotes_offline_live_and_mapping_holders(tmp_path: Path) -> None:
    registry = _make_registry(tmp_path)
    old_id = await _register(
        registry,
        path="/tmp/orchestrator-2",
        role=PeerRole.ORCHESTRATOR,
    )
    active_id = await _register(registry, path="/tmp/orchestrator")
    async with registry._lock:
        registry._peers[old_id].status = PeerStatus.OFFLINE

    result = await registry.claim_special_role(active_id, PeerRole.ORCHESTRATOR)

    assert result is not None
    assert result.peer.peer_id == active_id
    assert result.previous_holders[0]["peer_id"] == old_id
    assert registry.get_mapping(active_id).role == PeerRole.ORCHESTRATOR
    assert registry.get_mapping(old_id).role == PeerRole.AGENT
    old_peer = await registry.get_peer(old_id)
    assert old_peer is not None
    assert old_peer.role == PeerRole.AGENT
    event = registry.get_events()[-1]
    assert event["type"] == "role_claimed"
    assert event["peer_id"] == active_id


@pytest.mark.asyncio
async def test_claim_orchestrator_demotes_mapping_only_holder(tmp_path: Path) -> None:
    registry = _make_registry(tmp_path)
    old_id = await _register(
        registry,
        path="/tmp/orchestrator-2",
        role=PeerRole.ORCHESTRATOR,
    )
    active_id = await _register(registry, path="/tmp/orchestrator")
    async with registry._lock:
        del registry._peers[old_id]

    result = await registry.claim_special_role(active_id, PeerRole.ORCHESTRATOR)

    assert result is not None
    assert registry.get_mapping(active_id).role == PeerRole.ORCHESTRATOR
    assert registry.get_mapping(old_id).role == PeerRole.AGENT
    assert result.previous_holders[0]["status"] == "mapping-only"


@pytest.mark.asyncio
async def test_claim_orchestrator_rejects_fresh_holder_without_force(tmp_path: Path) -> None:
    registry = _make_registry(tmp_path)
    await _register(registry, path="/tmp/orchestrator-2", role=PeerRole.ORCHESTRATOR)
    active_id = await _register(registry, path="/tmp/orchestrator")

    with pytest.raises(RoleClaimConflictError, match="already held"):
        await registry.claim_special_role(active_id, PeerRole.ORCHESTRATOR)


@pytest.mark.asyncio
async def test_claim_orchestrator_allows_stale_holder(tmp_path: Path) -> None:
    registry = _make_registry(tmp_path)
    old_id = await _register(
        registry,
        path="/tmp/orchestrator-2",
        role=PeerRole.ORCHESTRATOR,
    )
    active_id = await _register(registry, path="/tmp/orchestrator")
    async with registry._lock:
        registry._peers[old_id].last_seen = (
            datetime.now(timezone.utc) - timedelta(seconds=90)
        )

    result = await registry.claim_special_role(active_id, PeerRole.ORCHESTRATOR)

    assert result is not None
    assert registry.get_mapping(active_id).role == PeerRole.ORCHESTRATOR
    assert registry.get_mapping(old_id).role == PeerRole.AGENT


@pytest.fixture
async def client(tmp_path: Path):
    registry = _make_registry(tmp_path)
    cfg = registry._config
    state = SimpleNamespace(
        config=cfg,
        transport=registry._transport,
        query_tracker=registry._query_tracker,
        message_router=registry._router,
        peer_registry=registry,
        relay_mode=False,
    )
    init_deps(cfg, registry, state)
    app = FastAPI()
    app.include_router(peers.router)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as c:
        yield c, registry
    cleanup_deps()


@pytest.mark.asyncio
async def test_claim_role_route_returns_clear_conflict(
    client: tuple[AsyncClient, PeerRegistry],
) -> None:
    http_client, registry = client
    await _register(registry, path="/tmp/orchestrator-2", role=PeerRole.ORCHESTRATOR)
    active_id = await _register(registry, path="/tmp/orchestrator")

    resp = await http_client.post(
        "/peers/claim-role",
        json={"role": "orchestrator", "peer_name": active_id},
    )

    assert resp.status_code == 409
    assert "already held" in resp.json()["detail"]


def _mock_client(monkeypatch: pytest.MonkeyPatch, response: MagicMock) -> MagicMock:
    client = MagicMock()
    client.post.return_value = response
    client.__enter__.return_value = client
    monkeypatch.setattr("httpx.Client", MagicMock(return_value=client))
    monkeypatch.setattr("repowire.cli._get_daemon_url", lambda: "http://127.0.0.1:8377")
    return client


def test_cli_claim_role_posts_existing_peer(monkeypatch: pytest.MonkeyPatch) -> None:
    response = MagicMock()
    response.status_code = 200
    response.raise_for_status.return_value = None
    response.json.return_value = {
        "peer_id": "repow-ops-active",
        "peer_name": "orchestrator-codex",
        "role": "orchestrator",
        "circle": "ops",
        "previous_holders": [{"peer_id": "repow-ops-old"}],
    }
    client = _mock_client(monkeypatch, response)

    result = CliRunner().invoke(
        main,
        ["peer", "claim-role", "orchestrator", "--peer", "orchestrator-codex", "--circle", "ops"],
    )

    assert result.exit_code == 0, result.output
    client.post.assert_called_once_with(
        "http://127.0.0.1:8377/peers/claim-role",
        json={
            "role": "orchestrator",
            "peer_name": "orchestrator-codex",
            "circle": "ops",
            "force": False,
        },
    )
    assert "Claimed role=orchestrator" in result.output
    assert "Demoted 1 previous holder" in result.output


def test_cli_claim_role_reports_live_holder_conflict(monkeypatch: pytest.MonkeyPatch) -> None:
    response = MagicMock()
    response.status_code = 409
    response.json.return_value = {
        "detail": (
            "role=orchestrator is already held by old "
            "(repow-ops-old) in circle ops"
        ),
    }
    _mock_client(monkeypatch, response)

    result = CliRunner().invoke(
        main,
        ["peer", "claim-role", "orchestrator", "--peer", "orchestrator-codex"],
    )

    assert result.exit_code == 1
    assert "Cannot claim role" in result.output
    assert "Use --force" in result.output
