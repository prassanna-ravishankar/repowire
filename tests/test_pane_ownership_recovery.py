"""Regressions for sticky-orchestrator pane ownership.

A temporary peer that registers on a pane already held by a live
orchestrator must not displace the orchestrator's pane bookkeeping or
liveness. Backend never switches inside a single peer_id; the failure
class these tests pin down is destructive pane transfer + no recovery
on the temp peer's exit, which left the original orchestrator with
broken transport/window routing when the temp peer closed.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from repowire.config.models import AgentType, Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
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


PANE = "%42"


@pytest.mark.asyncio
async def test_sticky_orchestrator_pane_blocks_displacement(tmp_path: Path) -> None:
    """A fresh codex orchestrator must keep pane_id, status, role when a
    different-path claude registers on the same pane. The claude is
    registered (so it can still call MCP outbound) but pane_id=None."""
    registry = _make_registry(tmp_path)
    orch_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/.repowire/orchestrator",
        pane_id=PANE,
        role=PeerRole.ORCHESTRATOR,
        machine="m",
    )

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/temp",
        pane_id=PANE,
        machine="m",
    )

    orch = await registry.get_peer(orch_id)
    claim = await registry.get_peer(claim_id)
    assert orch is not None and claim is not None
    assert orch.pane_id == PANE
    assert orch.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
    assert orch.role == PeerRole.ORCHESTRATOR
    assert claim.pane_id is None
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == orch_id


@pytest.mark.asyncio
async def test_same_path_orchestrator_restart_reuses_sticky_identity(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """A same-workspace SessionStart that omitted role/peer_id is a reconnect,
    not a new pane-less agent twin."""
    registry = _make_registry(tmp_path)
    config_dir = tmp_path / "cfg"
    monkeypatch.setattr(
        "repowire.config.paths.get_config_dir",
        lambda: config_dir,
    )
    path = str(config_dir / "orchestrator")
    orch_id, orch_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=path,
        pane_id=PANE,
        role=PeerRole.ORCHESTRATOR,
        machine="m",
    )

    claim_id, claim_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=path,
        pane_id=PANE,
        machine="m2",
        agent_pid=222,
        metadata={"hook_session_id": "restart-session"},
    )

    assert claim_id == orch_id
    assert claim_name == orch_name
    peers = await registry.get_all_peers()
    assert [p.display_name for p in peers] == [orch_name]
    orch = await registry.get_peer(orch_id)
    assert orch is not None
    assert orch.role == PeerRole.ORCHESTRATOR
    assert orch.pane_id == PANE
    assert orch.agent_pid == 222
    assert orch.metadata["hook_session_id"] == "restart-session"


@pytest.mark.asyncio
async def test_same_path_non_config_orchestrator_does_not_reuse_sticky_identity(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """A peer cannot become/reuse orchestrator authority just by path/name shape."""
    registry = _make_registry(tmp_path)
    config_dir = tmp_path / "cfg"
    monkeypatch.setattr(
        "repowire.config.paths.get_config_dir",
        lambda: config_dir,
    )
    wrong_path = str(tmp_path / "other" / "orchestrator")
    orch_id, orch_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=wrong_path,
        pane_id=PANE,
        role=PeerRole.ORCHESTRATOR,
        machine="m",
    )

    claim_id, claim_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=wrong_path,
        pane_id=PANE,
        machine="m2",
    )

    assert claim_id != orch_id
    assert claim_name != orch_name
    holder = await registry.get_peer(orch_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id == PANE
    assert holder.role == PeerRole.ORCHESTRATOR
    assert claim.pane_id is None
    assert claim.role == PeerRole.AGENT


@pytest.mark.asyncio
async def test_temp_peer_exit_leaves_orchestrator_intact(tmp_path: Path) -> None:
    """After the temp peer goes OFFLINE (its WS closes), the orchestrator
    still owns the pane and is reachable -- no manual repair needed."""
    registry = _make_registry(tmp_path)
    orch_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/.repowire/orchestrator",
        pane_id=PANE,
        role=PeerRole.ORCHESTRATOR,
        machine="m",
    )
    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/temp",
        pane_id=PANE,
        machine="m",
    )

    await registry.mark_offline(claim_id)

    orch = await registry.get_peer(orch_id)
    assert orch is not None
    assert orch.pane_id == PANE
    assert orch.role == PeerRole.ORCHESTRATOR
    assert orch.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == orch_id


@pytest.mark.asyncio
async def test_non_orchestrator_incumbent_still_displaces(tmp_path: Path) -> None:
    """Sticky behavior is scoped to ORCHESTRATOR. A new SessionStart on a
    pane held by a regular agent is still a real takeover (current
    semantics: pane re-bound, prior peer marked OFFLINE)."""
    registry = _make_registry(tmp_path)
    old_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/foo",
        pane_id=PANE,
        machine="m",
    )

    new_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/bar",
        pane_id=PANE,
        machine="m",
    )

    old = await registry.get_peer(old_id)
    new = await registry.get_peer(new_id)
    assert old is not None and new is not None
    assert old.pane_id is None
    assert old.status == PeerStatus.OFFLINE
    assert new.pane_id == PANE


@pytest.mark.asyncio
async def test_release_pane_preserves_orchestrator_liveness(tmp_path: Path) -> None:
    """Unit-level: _release_pane must not flip or detach a fresh ORCHESTRATOR.

    Pane ownership is sticky for the orchestrator. A temporary claimant on
    the same pane must not leave the live orchestrator online but pane-less.
    """
    registry = _make_registry(tmp_path)
    orch_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/.repowire/orchestrator",
        pane_id=PANE,
        role=PeerRole.ORCHESTRATOR,
        machine="m",
    )

    async with registry._lock:
        registry._release_pane(PANE, new_peer_id="some-other-id")

    orch = await registry.get_peer(orch_id)
    assert orch is not None
    assert orch.pane_id == PANE
    assert orch.status in (PeerStatus.ONLINE, PeerStatus.BUSY)  # liveness preserved
    assert orch.role == PeerRole.ORCHESTRATOR


@pytest.mark.asyncio
async def test_register_route_reports_unassigned_sticky_orchestrator_pane(
    tmp_path: Path,
) -> None:
    """HTTP registration must expose pane_assigned=false on sticky refusal.

    The hook consumes this wire field to avoid killing the incumbent ws-hook
    or clearing pane runtime metadata.
    """
    from types import SimpleNamespace

    from fastapi import FastAPI
    from httpx import ASGITransport, AsyncClient

    from repowire.daemon.deps import cleanup_deps, init_deps
    from repowire.daemon.routes import peers

    registry = _make_registry(tmp_path)
    cfg = Config()
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
    try:
        orch_id, _ = await registry.allocate_and_register(
            circle="default",
            backend=AgentType.CODEX,
            path="/home/u/.repowire/orchestrator",
            pane_id=PANE,
            role=PeerRole.ORCHESTRATOR,
            machine="m",
        )
        async with AsyncClient(
            transport=ASGITransport(app=app), base_url="http://test",
        ) as client:
            response = await client.post(
                "/peers",
                json={
                    "name": "temp",
                    "path": "/home/u/projects/temp",
                    "circle": "default",
                    "backend": "claude-code",
                    "pane_id": PANE,
                },
            )
        assert response.status_code == 200
        body = response.json()
        assert body["pane_assigned"] is False

        by_pane = await registry.get_peer_by_pane(PANE)
        assert by_pane is not None
        assert by_pane.peer_id == orch_id
        temp = await registry.get_peer(body["peer_id"])
        assert temp is not None
        assert temp.pane_id is None
    finally:
        cleanup_deps()


@pytest.mark.asyncio
async def test_release_pane_still_offlines_agent(tmp_path: Path) -> None:
    """Sanity: the preservation rule is scoped to ORCHESTRATOR; regular
    agents are still marked OFFLINE when their pane gets reassigned."""
    registry = _make_registry(tmp_path)
    agent_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/foo",
        pane_id=PANE,
        machine="m",
    )

    async with registry._lock:
        registry._release_pane(PANE, new_peer_id="some-other-id")

    agent = await registry.get_peer(agent_id)
    assert agent is not None
    assert agent.pane_id is None
    assert agent.status == PeerStatus.OFFLINE
