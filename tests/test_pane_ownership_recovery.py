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
from repowire.hooks.utils import read_pane_runtime_metadata, write_pane_runtime_metadata
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
async def test_same_pane_displacement_rewrites_hook_metadata(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("repowire.config.models.CACHE_DIR", tmp_path / "cache")
    registry = _make_registry(tmp_path)
    old_id, old_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/foo",
        pane_id=PANE,
        machine="m",
    )
    write_pane_runtime_metadata(
        PANE,
        {
            "agent_pid": 111,
            "backend": "codex",
            "birth_certificate": {"peer_id": old_id},
            "cwd": "/home/u/projects/foo",
            "display_name": old_name,
            "hook_session_id": "hook-session-1",
            "peer_id": old_id,
        },
    )

    new_id, new_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/bar",
        pane_id=PANE,
        machine="m",
    )

    meta = read_pane_runtime_metadata(PANE)
    assert meta["peer_id"] == new_id
    assert meta["display_name"] == new_name
    assert meta["agent_pid"] == 111
    assert meta["hook_session_id"] == "hook-session-1"
    assert "birth_certificate" not in meta


@pytest.mark.asyncio
async def test_display_name_update_rewrites_hook_metadata(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("repowire.config.models.CACHE_DIR", tmp_path / "cache")
    registry = _make_registry(tmp_path)
    peer_id, name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/foo",
        pane_id=PANE,
        machine="m",
    )
    write_pane_runtime_metadata(
        PANE,
        {
            "backend": "codex",
            "birth_certificate": {"peer_id": peer_id},
            "cwd": "/home/u/projects/foo",
            "display_name": name,
            "peer_id": peer_id,
        },
    )

    assert await registry.update_peer_display_name(peer_id, "renamed-codex")

    meta = read_pane_runtime_metadata(PANE)
    assert meta["peer_id"] == peer_id
    assert meta["display_name"] == "renamed-codex"
    assert meta["birth_certificate"] == {"peer_id": peer_id}


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


# ---------------------------------------------------------------------------
# Destructive pane-claim proof (repowire-oyg)
#
# A headless subprocess agent (e.g. `codex exec` launched from inside a
# registered claude-code session) inherits TMUX_PANE and used to displace the
# live pane holder on registration. Displacing a live holder now requires
# proof the claimant actually runs in the pane: its ancestor chain must reach
# the pane's root pid without passing through the live holder's agent.
# ---------------------------------------------------------------------------

HOLDER_PID = 1000
CLAIMANT_PID = 2000
PANE_PID = 400


def _patch_probes(
    monkeypatch: pytest.MonkeyPatch,
    *,
    ancestors: set[int] | None,
    pane_pid: int | None,
    holder_alive: bool = True,
) -> None:
    monkeypatch.setattr(
        "repowire.daemon.peer_registry.process_ancestors", lambda pid: ancestors
    )
    monkeypatch.setattr(
        "repowire.daemon.peer_registry.tmux_pane_pid", lambda pane: pane_pid
    )
    monkeypatch.setattr(
        "repowire.daemon.peer_registry.pid_alive", lambda pid: holder_alive
    )


async def _register_holder(registry: PeerRegistry) -> str:
    holder_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=HOLDER_PID,
    )
    return holder_id


@pytest.mark.asyncio
async def test_live_holder_kept_when_claimant_not_in_pane_tree(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A claimant whose process tree does not reach the pane's root pid
    cannot displace a live holder: it registers pane-less and a loud
    pane_claim_rejected event is emitted."""
    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    # Claimant's ancestry never reaches the pane root (it merely inherited
    # TMUX_PANE in its env).
    _patch_probes(monkeypatch, ancestors={500, 1}, pane_pid=PANE_PID)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id == PANE
    assert holder.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
    assert claim.pane_id is None
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == holder_id
    rejected = [e for e in registry.get_events() if e["type"] == "pane_claim_rejected"]
    assert len(rejected) == 1
    assert rejected[0]["holder_peer_id"] == holder_id
    assert rejected[0]["claimant_agent_pid"] == CLAIMANT_PID


@pytest.mark.asyncio
async def test_live_holder_kept_when_claimant_is_holder_subprocess(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The repowire-oyg incident: a headless agent launched from inside the
    holder's session descends from the holder's agent pid. Even though its
    chain also reaches the pane root, it must not take the pane."""
    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    # claimant <- shell <- holder agent <- pane root
    _patch_probes(monkeypatch, ancestors={1500, HOLDER_PID, PANE_PID, 1}, pane_pid=PANE_PID)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id == PANE
    assert holder.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
    assert claim.pane_id is None
    rejected = [e for e in registry.get_events() if e["type"] == "pane_claim_rejected"]
    assert len(rejected) == 1
    assert "subprocess" in rejected[0]["reason"]


@pytest.mark.asyncio
async def test_live_holder_displaced_by_pane_descendant_claimant(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A claimant that genuinely runs in the pane (ancestry reaches pane_pid
    without passing through the holder's agent) may still take the pane from
    a live holder — e.g. a new agent started in the pane's shell."""
    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    _patch_probes(monkeypatch, ancestors={PANE_PID, 1}, pane_pid=PANE_PID)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/other",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id is None
    assert holder.status == PeerStatus.OFFLINE
    assert claim.pane_id == PANE
    assert not [e for e in registry.get_events() if e["type"] == "pane_claim_rejected"]


@pytest.mark.asyncio
async def test_stale_holder_displaced_without_proof(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A heartbeat-stale holder is the legitimate pane-reuse case: takeover
    proceeds even when the claimant offers no pane proof at all."""
    from datetime import datetime, timedelta, timezone

    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    holder = await registry.get_peer(holder_id)
    assert holder is not None
    holder.last_seen = datetime.now(timezone.utc) - timedelta(
        seconds=registry.heartbeat_tolerance() * 4
    )
    # Probes would reject this claimant if the guard consulted them.
    _patch_probes(monkeypatch, ancestors={500, 1}, pane_pid=PANE_PID)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/other",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    claim = await registry.get_peer(claim_id)
    assert claim is not None and claim.pane_id == PANE
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == claim_id


@pytest.mark.asyncio
async def test_dead_agent_holder_displaced_without_proof(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A fresh-looking holder whose agent process is gone (restart race) is
    not a live holder; same-pane relaunch takes the pane as before."""
    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    _patch_probes(
        monkeypatch, ancestors={500, 1}, pane_pid=PANE_PID, holder_alive=False
    )

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert claim is not None and claim.pane_id == PANE
    if holder is not None and holder.peer_id != claim_id:
        assert holder.pane_id is None


@pytest.mark.asyncio
async def test_inconclusive_probes_allow_takeover(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Probe failure (no ps/tmux answer) is 'can't decide' and lets the claim
    through — the pre-guard behavior, matching the parent_pid guard's
    safe default."""
    registry = _make_registry(tmp_path)
    holder_id = await _register_holder(registry)
    _patch_probes(monkeypatch, ancestors=None, pane_pid=None)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path="/home/u/projects/other",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id is None
    assert claim.pane_id == PANE


@pytest.mark.asyncio
async def test_rejected_claimant_never_adopts_holder_identity_cross_circle(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A rejected same-name/backend/path claimant must mint a NEW identity,
    not adopt the live holder's mapping. The cross-circle adoption branch
    (claimant in fallback 'default' circle, holder elsewhere) used to hand
    back the holder's session id, and the final Peer write then overwrote
    the holder's record pane-less under its own peer_id."""
    registry = _make_registry(tmp_path)
    holder_id, holder_name = await registry.allocate_and_register(
        circle="work",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=HOLDER_PID,
    )
    _patch_probes(monkeypatch, ancestors={500, 1}, pane_pid=PANE_PID)

    claim_id, _ = await registry.allocate_and_register(
        circle="default",  # fallback circle -> cross-circle adoption eligible
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
    )

    assert claim_id != holder_id
    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id == PANE
    assert holder.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
    assert holder.agent_pid == HOLDER_PID
    assert holder.display_name == holder_name
    assert claim.pane_id is None
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == holder_id
    rejected = [e for e in registry.get_events() if e["type"] == "pane_claim_rejected"]
    assert len(rejected) == 1
    assert rejected[0]["outcome"] == "registered_pane_less"


@pytest.mark.asyncio
async def test_rejected_same_session_claimant_does_not_evict_holder(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Same circle, same name, and a matching runtime session id: the
    display-name reclaim path used to DELETE the live holder outright. A
    rejected claimant must fall through to a suffixed fresh identity."""
    registry = _make_registry(tmp_path)
    holder_id, holder_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=HOLDER_PID,
        metadata={"hook_session_id": "sess-1"},
    )
    _patch_probes(monkeypatch, ancestors={500, 1}, pane_pid=PANE_PID)

    claim_id, claim_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=CLAIMANT_PID,
        metadata={"hook_session_id": "sess-1"},  # orphan duplicate claim
    )

    assert claim_id != holder_id
    assert claim_name != holder_name
    holder = await registry.get_peer(holder_id)
    claim = await registry.get_peer(claim_id)
    assert holder is not None and claim is not None
    assert holder.pane_id == PANE
    assert holder.display_name == holder_name
    assert claim.pane_id is None
    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == holder_id


@pytest.mark.asyncio
async def test_direct_child_hijack_still_409s_and_emits_event(
    tmp_path: Path,
) -> None:
    """The direct-child SessionStart hijack keeps its hard 409 (hook versions
    between the #190 guard and the pane_assigned contract treat any 2xx as a
    confirmed claim and run the destructive client-side takeover), but now
    also emits the same pane_claim_rejected event for observability."""
    from repowire.daemon.peer_registry import PaneHijackRejectedError

    registry = _make_registry(tmp_path)
    holder_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/home/u/projects/main",
        pane_id=PANE,
        machine="m",
        agent_pid=HOLDER_PID,
    )

    with pytest.raises(PaneHijackRejectedError):
        await registry.allocate_and_register(
            circle="default",
            backend=AgentType.GEMINI,
            path="/home/u/projects/main",
            pane_id=PANE,
            machine="m",
            agent_pid=CLAIMANT_PID,
            parent_pid=HOLDER_PID,
        )

    by_pane = await registry.get_peer_by_pane(PANE)
    assert by_pane is not None and by_pane.peer_id == holder_id
    rejected = [e for e in registry.get_events() if e["type"] == "pane_claim_rejected"]
    assert len(rejected) == 1
    assert rejected[0]["outcome"] == "registration_rejected"
    assert rejected[0]["holder_peer_id"] == holder_id


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
