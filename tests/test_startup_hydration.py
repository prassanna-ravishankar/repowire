from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from repowire.agent_types import AgentType
from repowire.config.models import Config
from repowire.daemon.message_router import MessageRouter
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.query_tracker import QueryTracker
from repowire.daemon.registry_identity import SessionMapping
from repowire.daemon.startup_hydration import hydrate_startup_peers
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.protocol.peers import PeerRole, PeerStatus

PEER_ID = "repow-default-abc12345"
PANE_ID = "%42"
PATH = "/tmp/repowire-project"
AGENT_PID = 12345


def _pane(
    *,
    pane_id: str = PANE_ID,
    cwd: str = PATH,
    pid: int = 999,
) -> dict:
    return {
        "pane_id": pane_id,
        "pid": pid,
        "command": "codex",
        "cwd": cwd,
        "session": "default",
        "window": "1",
    }


def _metadata(**overrides):
    data = {
        "peer_id": PEER_ID,
        "display_name": "repowire-project-codex",
        "backend": "codex",
        "cwd": PATH,
        "agent_pid": AGENT_PID,
        "parent_pid": 222,
        "hook_session_id": "runtime-1",
    }
    data.update(overrides)
    return data


def _registry(tmp_path):
    transport = WebSocketTransport()
    tracker = QueryTracker()
    router = MessageRouter(transport=transport, query_tracker=tracker)
    db = StateDatabase(tmp_path / "state.db")
    registry = PeerRegistry(
        config=Config(),
        message_router=router,
        query_tracker=tracker,
        transport=transport,
        persistence_path=tmp_path / "sessions.json",
        state_db=db,
    )
    registry._events_path = tmp_path / "events.json"
    registry._events.clear()
    return registry, transport, SQLiteSessionBindingStore(db)


def _add_mapping(registry: PeerRegistry, *, peer_id: str = PEER_ID) -> None:
    registry._mappings[peer_id] = SessionMapping(
        session_id=peer_id,
        display_name="repowire-project-codex",
        circle="default",
        backend=AgentType.CODEX,
        path=PATH,
        role=PeerRole.AGENT,
        model="gpt-5.5",
        agent_pid=AGENT_PID,
    )


def _mapping(
    *,
    peer_id: str,
    display_name: str,
    path: str,
    agent_pid: int,
) -> SessionMapping:
    return SessionMapping(
        session_id=peer_id,
        display_name=display_name,
        circle="default",
        backend=AgentType.CODEX,
        path=path,
        role=PeerRole.AGENT,
        model="gpt-5.5",
        agent_pid=agent_pid,
    )


@pytest.mark.asyncio
async def test_mapping_alone_does_not_hydrate(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)

    with patch("repowire.daemon.startup_hydration.list_all_panes", return_value=[]):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert await registry.get_peer(PEER_ID) is None


@pytest.mark.asyncio
async def test_matching_live_pane_hydrates_offline_and_reports_no_transport(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch("repowire.daemon.peer_registry.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=False,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 1
    assert result.no_transport == 1
    respawn.assert_called_once()
    peer = await registry.get_peer(PEER_ID)
    assert peer is not None
    assert peer.status == PeerStatus.OFFLINE
    assert peer.pane_id == PANE_ID
    assert peer.agent_pid == AGENT_PID
    assert peer.metadata["hydration_source"] == "startup_hydration"
    assert any(
        event["type"] == "startup_hydration_no_transport"
        and event["peer_id"] == PEER_ID
        for event in registry.get_events()
    )


@pytest.mark.asyncio
async def test_dead_agent_pid_does_not_hydrate_or_respawn(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=False),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert await registry.get_peer(PEER_ID) is None
    respawn.assert_not_called()


@pytest.mark.asyncio
async def test_retired_peer_without_live_pid_does_not_hydrate(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)
    registry._retire(PEER_ID)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(agent_pid=None),
        ),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert await registry.get_peer(PEER_ID) is None
    respawn.assert_not_called()


@pytest.mark.asyncio
async def test_retired_peer_with_live_pid_hydrates_and_unretires(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)
    registry._retire(PEER_ID)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch("repowire.daemon.peer_registry.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=False,
        ),
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 1
    assert PEER_ID not in registry._retired
    peer = await registry.get_peer(PEER_ID)
    assert peer is not None
    assert peer.status == PeerStatus.OFFLINE
    assert peer.agent_pid == AGENT_PID


@pytest.mark.asyncio
async def test_unproven_pane_does_not_respawn(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(peer_id=None, birth_certificate=None),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert result.skipped == 1
    respawn.assert_not_called()
    assert any(
        event["type"] == "startup_hydration_skipped"
        and event["reason"] == "missing_peer_id"
        for event in registry.get_events()
    )


@pytest.mark.asyncio
async def test_connected_peer_wins_hydration_noop(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)
    await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path=PATH,
        pane_id=PANE_ID,
        role=PeerRole.AGENT,
        peer_id=PEER_ID,
        initial_status=PeerStatus.ONLINE,
        agent_pid=AGENT_PID,
        display_name_override="repowire-project-codex",
    )
    websocket = AsyncMock()
    await transport.connect(PEER_ID, websocket, pane_id=PANE_ID)

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert result.connected == 1
    respawn.assert_not_called()
    peer = await registry.get_peer(PEER_ID)
    assert peer is not None
    assert peer.status == PeerStatus.ONLINE


@pytest.mark.asyncio
async def test_existing_online_peer_without_transport_is_not_downgraded(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)
    await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CODEX,
        path=PATH,
        pane_id=PANE_ID,
        role=PeerRole.AGENT,
        peer_id=PEER_ID,
        initial_status=PeerStatus.ONLINE,
        agent_pid=AGENT_PID,
        display_name_override="repowire-project-codex",
    )

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert result.no_transport == 0
    respawn.assert_not_called()
    peer = await registry.get_peer(PEER_ID)
    assert peer is not None
    assert peer.status == PeerStatus.ONLINE


@pytest.mark.asyncio
async def test_valid_certificate_can_hydrate_without_metadata_peer_id(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)
    cert = store.mint_birth_certificate(
        peer_id=PEER_ID,
        display_name="repowire-project-codex",
        backend=AgentType.CODEX,
        project_path=PATH,
        runtime_session_id="runtime-1",
        pane_id=PANE_ID,
        agent_pid=AGENT_PID,
        parent_pid=222,
        metadata={"circle": "default", "role": "agent"},
    )

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[_pane()],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            return_value=_metadata(peer_id=None, birth_certificate=cert.as_envelope()),
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=False,
        ),
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 1
    peer = await registry.get_peer(PEER_ID)
    assert peer is not None
    assert peer.metadata["birth_certificate_nonce"] == cert.nonce


@pytest.mark.asyncio
async def test_respawn_limit_bounds_startup_hook_spawns(tmp_path):
    registry, transport, store = _registry(tmp_path)
    second_peer_id = "repow-default-def67890"
    second_path = "/tmp/repowire-project-2"
    second_pid = 23456
    registry._mappings[PEER_ID] = _mapping(
        peer_id=PEER_ID,
        display_name="repowire-project-codex",
        path=PATH,
        agent_pid=AGENT_PID,
    )
    registry._mappings[second_peer_id] = _mapping(
        peer_id=second_peer_id,
        display_name="repowire-project-2-codex",
        path=second_path,
        agent_pid=second_pid,
    )

    def metadata_for(pane_id: str):
        if pane_id == PANE_ID:
            return _metadata()
        return _metadata(
            peer_id=second_peer_id,
            display_name="repowire-project-2-codex",
            cwd=second_path,
            agent_pid=second_pid,
        )

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[
                _pane(),
                _pane(pane_id="%43", cwd=second_path, pid=1000),
            ],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            side_effect=metadata_for,
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
            respawn_limit=1,
        )

    assert result.hydrated == 2
    assert result.respawned == 1
    assert respawn.call_count == 1


@pytest.mark.asyncio
async def test_duplicate_peer_claim_skips_all_claimants_and_does_not_respawn(tmp_path):
    registry, transport, store = _registry(tmp_path)
    _add_mapping(registry)

    def metadata_for(_pane_id: str):
        return _metadata()

    with (
        patch(
            "repowire.daemon.startup_hydration.list_all_panes",
            return_value=[
                _pane(),
                _pane(pane_id="%43", cwd=PATH, pid=1000),
            ],
        ),
        patch(
            "repowire.daemon.startup_hydration.read_pane_runtime_metadata",
            side_effect=metadata_for,
        ),
        patch("repowire.daemon.startup_hydration.pid_alive", return_value=True),
        patch(
            "repowire.hooks.ws_hook_supervisor.startup_respawn_ws_hook",
            return_value=True,
        ) as respawn,
    ):
        result = await hydrate_startup_peers(
            registry=registry,
            transport=transport,
            binding_store=store,
            connect_wait_seconds=0,
        )

    assert result.hydrated == 0
    assert result.skipped == 2
    assert await registry.get_peer(PEER_ID) is None
    respawn.assert_not_called()
    duplicate_events = [
        event
        for event in registry.get_events()
        if event["type"] == "startup_hydration_skipped"
        and event["reason"] == "duplicate_peer_claim"
    ]
    assert {event["pane_id"] for event in duplicate_events} == {PANE_ID, "%43"}
