"""Tests for PeerRegistry session mapping persistence and pruning."""

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from repowire.config.models import AgentType
from repowire.daemon.peer_registry import PeerRegistry
from repowire.protocol.peers import PeerRole, PeerStatus


def _make_registry(tmp_path: Path, mappings: dict | None = None) -> PeerRegistry:
    path = tmp_path / "sessions.json"
    if mappings:
        path.write_text(json.dumps(mappings, indent=2))
    return PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )


def _ts(hours_ago: float) -> str:
    return (datetime.now(timezone.utc) - timedelta(hours=hours_ago)).isoformat()


def test_prune_removes_old_mappings(tmp_path):
    mappings = {
        "repow-dev-old1": {
            "session_id": "repow-dev-old1",
            "display_name": "old1",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(100),
        },
        "repow-dev-recent": {
            "session_id": "repow-dev-recent",
            "display_name": "recent",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(1),
        },
    }
    registry = _make_registry(tmp_path, mappings)
    pruned = registry.prune_offline(max_age_hours=72)
    assert pruned == 1
    assert registry.get_mapping("repow-dev-old1") is None
    assert registry.get_mapping("repow-dev-recent") is not None


def test_prune_removes_entries_with_no_timestamp(tmp_path):
    mappings = {
        "repow-dev-notimestamp": {
            "session_id": "repow-dev-notimestamp",
            "display_name": "notimestamp",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": None,
        },
    }
    path = tmp_path / "sessions.json"
    path.write_text(json.dumps(mappings, indent=2))
    registry = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )
    # __post_init__ sets updated_at, so this entry is fresh — won't be pruned
    assert registry.prune_offline() == 0


def test_prune_persists_to_disk(tmp_path):
    mappings = {
        "repow-dev-stale": {
            "session_id": "repow-dev-stale",
            "display_name": "stale",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(200),
        },
    }
    registry = _make_registry(tmp_path, mappings)
    registry.prune_offline()
    # Force flush to disk
    registry._mappings_dirty = True
    registry._persist_mappings()
    # Reload from disk
    registry2 = _make_registry(tmp_path)
    assert registry2.get_mapping("repow-dev-stale") is None


def test_prune_removes_entries_with_bad_timestamp(tmp_path):
    mappings = {
        "repow-dev-badtimestamp": {
            "session_id": "repow-dev-badtimestamp",
            "display_name": "badtimestamp",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": "not-a-valid-iso-timestamp",
        },
    }
    registry = _make_registry(tmp_path, mappings)
    assert registry.prune_offline() == 1
    assert registry.get_mapping("repow-dev-badtimestamp") is None


def test_prune_noop_when_nothing_stale(tmp_path):
    mappings = {
        "repow-dev-fresh": {
            "session_id": "repow-dev-fresh",
            "display_name": "fresh",
            "circle": "dev",
            "backend": AgentType.CLAUDE_CODE,
            "updated_at": _ts(1),
        },
    }
    registry = _make_registry(tmp_path, mappings)
    assert registry.prune_offline() == 0


@pytest.mark.asyncio
async def test_new_pane_registration_clears_old_peer_pane_id(tmp_path):
    """When a new session registers for a pane, the old peer's pane_id is cleared.

    Reproduces the session-restart bug: old ws-hook dies, new session starts in
    the same pane, registers successfully. Without pane eviction, get_peer_by_pane
    returns the stale old peer. With _release_pane, the old peer loses its pane_id
    so only the new peer is reachable by pane.
    """
    registry = _make_registry(tmp_path)

    # Old session registers with pane %1
    old_id, old_name = await registry.allocate_and_register(
        circle="dev", backend=AgentType.CLAUDE_CODE,
        path="/tmp/oldsess", pane_id="%1",
    )
    old_pane_peer = await registry.get_peer_by_pane("%1")
    assert old_pane_peer is not None and old_pane_peer.peer_id == old_id

    # New session starts in the same pane after old ws-hook dies
    new_id, new_name = await registry.allocate_and_register(
        circle="dev", backend=AgentType.CLAUDE_CODE,
        path="/tmp/newsess", pane_id="%1",
    )

    # Pane now resolves to the new session
    peer = await registry.get_peer_by_pane("%1")
    assert peer is not None
    assert peer.peer_id == new_id

    # Old peer still exists but no longer owns the pane, and is marked
    # offline so it can't be claimed by future MCP path+backend lookups.
    old_peer = await registry.get_peer(old_name)
    assert old_peer is not None
    assert old_peer.pane_id is None
    assert old_peer.status == PeerStatus.OFFLINE


@pytest.mark.asyncio
async def test_lookup_prefers_pane_owned_peer_when_names_collide(tmp_path):
    """Name lookup should prefer the live pane-owned peer over a generic duplicate."""
    registry = _make_registry(tmp_path)

    await registry.allocate_and_register(
        circle="default", backend=AgentType.CODEX, path="/tmp/repowire",
    )
    pane_peer_id, _pane_name = await registry.allocate_and_register(
        circle="0", backend=AgentType.CODEX, path="/tmp/repowire", pane_id="%1",
    )

    peer = await registry.get_peer("repowire-codex")
    assert peer is not None
    assert peer.peer_id == pane_peer_id


@pytest.mark.asyncio
async def test_stale_peer_id_claim_does_not_steal_existing_peer(tmp_path):
    """A stale REPOWIRE_PEER_ID must not bind a different pane to an old peer."""
    registry = _make_registry(tmp_path)

    sse_id, sse_name = await registry.allocate_and_register(
        circle="dev",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/repowire.feat-sse-improve",
        pane_id="%1",
    )

    claimed_id, claimed_name = await registry.allocate_and_register(
        circle="dev",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/repowire-orchestrator",
        pane_id="%2",
        peer_id=sse_id,
    )

    assert claimed_id != sse_id
    assert claimed_name != sse_name
    sse_peer = await registry.get_peer(sse_id)
    assert sse_peer is not None
    assert sse_peer.pane_id == "%1"
    claimed_peer = await registry.get_peer_by_pane("%2")
    assert claimed_peer is not None
    assert claimed_peer.peer_id == claimed_id


@pytest.mark.asyncio
async def test_stale_peer_id_claim_same_path_different_backend_does_not_steal_peer(tmp_path):
    """Same-cwd sibling agents must not be allowed to rebind each other's peer_id."""
    registry = _make_registry(tmp_path)
    shared_path = "/tmp/repowire-research-acp-claude"

    gemini_id, gemini_name = await registry.allocate_and_register(
        circle="dev",
        backend=AgentType.GEMINI,
        path=shared_path,
        pane_id="%1",
    )

    claimed_id, claimed_name = await registry.allocate_and_register(
        circle="dev",
        backend=AgentType.CLAUDE_CODE,
        path=shared_path,
        pane_id="%2",
        peer_id=gemini_id,
    )

    assert claimed_id != gemini_id
    assert claimed_name != gemini_name
    gemini_peer = await registry.get_peer(gemini_id)
    assert gemini_peer is not None
    assert gemini_peer.backend == AgentType.GEMINI
    assert gemini_peer.pane_id == "%1"
    claimed_peer = await registry.get_peer_by_pane("%2")
    assert claimed_peer is not None
    assert claimed_peer.peer_id == claimed_id
    assert claimed_peer.backend == AgentType.CLAUDE_CODE


@pytest.mark.asyncio
async def test_circle_and_description_persist_across_restart(tmp_path):
    """A peer re-registering after restart restores its prior circle + description.

    Reproduces issue #134: `claude --continue` loses tmux session context, so
    the new registration arrives with circle="default" even though the peer
    was previously moved to a non-default circle. The persisted mapping should
    bring both circle and description back without manual /peers/circle calls.
    """
    path = tmp_path / "sessions.json"
    orch_dir = tmp_path / "orchproj"
    orch_dir.mkdir()
    registry = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )

    peer_id, name = await registry.allocate_and_register(
        circle="5", backend=AgentType.CLAUDE_CODE, path=str(orch_dir),
    )
    await registry.update_description(peer_id, "watching the mesh")
    await registry.mark_offline(peer_id)
    # Flush to disk so a fresh registry can load it.
    registry._persist_mappings()

    # Simulate daemon restart: brand new registry reading the same file.
    registry2 = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )
    # claude --continue: tmux session name didn't propagate, so the hook
    # falls back to circle="default".
    new_id, new_name = await registry2.allocate_and_register(
        circle="default", backend=AgentType.CLAUDE_CODE, path=str(orch_dir),
    )
    assert new_name == name
    assert new_id == peer_id, "should adopt the prior session id"
    peer = await registry2.get_peer(new_id)
    assert peer is not None
    assert peer.circle == "5"
    assert peer.description == "watching the mesh"


@pytest.mark.asyncio
async def test_role_persists_across_restart_adoption(tmp_path):
    """A restarted daemon must not downgrade an adopted peer's persisted role."""
    path = tmp_path / "sessions.json"
    orch_dir = tmp_path / "orchproj"
    orch_dir.mkdir()
    registry = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )

    peer_id, name = await registry.allocate_and_register(
        circle="5",
        backend=AgentType.CLAUDE_CODE,
        path=str(orch_dir),
        role=PeerRole.ORCHESTRATOR,
    )
    registry._persist_mappings()

    # Simulate daemon restart: in-memory peers are gone, but sessions.json
    # still carries the role. The reconnecting ws-hook has no spawn hint and
    # therefore supplies the default role=agent.
    registry2 = PeerRegistry(
        config=__import__("repowire.config.models", fromlist=["Config"]).Config(),
        message_router=__import__("unittest.mock", fromlist=["MagicMock"]).MagicMock(),
        persistence_path=path,
    )
    new_id, new_name = await registry2.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(orch_dir),
        role=PeerRole.AGENT,
    )

    assert new_name == name
    assert new_id == peer_id
    peer = await registry2.get_peer(new_id)
    assert peer is not None
    assert peer.role == PeerRole.ORCHESTRATOR


@pytest.mark.asyncio
async def test_fresh_peer_with_no_prior_record_gets_defaults(tmp_path):
    """A brand new path/backend gets default circle and empty description."""
    registry = _make_registry(tmp_path)
    peer_id, _name = await registry.allocate_and_register(
        circle="default", backend=AgentType.CLAUDE_CODE, path="/tmp/freshproj",
    )
    peer = await registry.get_peer(peer_id)
    assert peer is not None
    assert peer.circle == "default"
    assert peer.description == ""


@pytest.mark.asyncio
async def test_non_default_circle_does_not_collapse_other_circle_peer(tmp_path):
    """Two peers at the same path in different non-default circles stay distinct.

    Adoption is gated on the incoming circle being "default" precisely to
    avoid this case: a second orchestrator registering with an explicit
    different circle must not be folded into the first one's mapping.
    """
    registry = _make_registry(tmp_path)
    first_id, _ = await registry.allocate_and_register(
        circle="A", backend=AgentType.CLAUDE_CODE, path="/tmp/shared",
    )
    second_id, _ = await registry.allocate_and_register(
        circle="B", backend=AgentType.CLAUDE_CODE, path="/tmp/shared",
    )
    assert first_id != second_id
    first_peer = await registry.get_peer(first_id)
    second_peer = await registry.get_peer(second_id)
    assert first_peer is not None and first_peer.circle == "A"
    assert second_peer is not None and second_peer.circle == "B"


@pytest.mark.asyncio
async def test_description_update_persists_to_mapping(tmp_path):
    """update_description writes through to the SessionMapping for durability."""
    registry = _make_registry(tmp_path)
    peer_id, _ = await registry.allocate_and_register(
        circle="default", backend=AgentType.CLAUDE_CODE, path="/tmp/descproj",
    )
    await registry.update_description(peer_id, "doing the thing")
    mapping = registry.get_mapping(peer_id)
    assert mapping is not None
    assert mapping.description == "doing the thing"


# ---------------------------------------------------------------------------
# Pane-hijack guard (issue #190)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_pane_hijack_rejected_when_parent_pid_matches_existing_agent(tmp_path):
    """A fresh SessionStart whose parent_pid matches the live pane peer's
    agent_pid is rejected — this is the subprocess-agent hijack case (e.g.
    ``gemini --yolo`` run from inside a claude-code pane inheriting TMUX_PANE)."""
    from repowire.daemon.peer_registry import PaneHijackRejectedError

    registry = _make_registry(tmp_path)

    parent_agent_pid = 11111
    parent_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/orchestrator",
        pane_id="%1",
        agent_pid=parent_agent_pid,
        parent_pid=99999,  # parent's own parent (a shell) — irrelevant here
    )

    with pytest.raises(PaneHijackRejectedError):
        await registry.allocate_and_register(
            circle="default",
            backend=AgentType.GEMINI,
            path="/tmp/some-other-cwd",
            pane_id="%1",
            agent_pid=22222,
            parent_pid=parent_agent_pid,  # gemini's parent IS the claude pid
        )

    # Original peer keeps the pane.
    peer = await registry.get_peer_by_pane("%1")
    assert peer is not None
    assert peer.peer_id == parent_id
    assert peer.backend == AgentType.CLAUDE_CODE


@pytest.mark.asyncio
async def test_relaunch_after_crash_allowed_when_existing_peer_is_stale(tmp_path):
    """If the prior peer's last_seen is older than the heartbeat tolerance,
    the guard does not fire — the prior agent is presumed dead."""
    registry = _make_registry(tmp_path)

    parent_agent_pid = 33333
    await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/oldproj",
        pane_id="%2",
        agent_pid=parent_agent_pid,
    )
    # Backdate the prior peer well beyond heartbeat tolerance.
    tolerance = registry.heartbeat_tolerance()
    stale_when = datetime.now(timezone.utc) - timedelta(seconds=tolerance * 4)
    for peer in registry._peers.values():
        if peer.pane_id == "%2":
            peer.last_seen = stale_when

    # New claude launches in the same pane. Even if (improbably) its ppid
    # collides with the dead agent's pid, the staleness lets it through.
    new_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/newproj",
        pane_id="%2",
        agent_pid=44444,
        parent_pid=parent_agent_pid,
    )
    new_peer = await registry.get_peer_by_pane("%2")
    assert new_peer is not None and new_peer.peer_id == new_id


@pytest.mark.asyncio
async def test_worktree_swap_in_same_pane_allowed(tmp_path):
    """`cd ~/other-proj && claude` in an existing pane: the new claude's
    parent is the shell, not the prior agent. Guard does not fire."""
    registry = _make_registry(tmp_path)

    await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/proj-a",
        pane_id="%3",
        agent_pid=55555,
    )

    new_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/proj-b",
        pane_id="%3",
        agent_pid=66666,
        parent_pid=77777,  # the shell's pid, not the prior agent's
    )
    peer = await registry.get_peer_by_pane("%3")
    assert peer is not None and peer.peer_id == new_id
    assert peer.path == "/tmp/proj-b"


@pytest.mark.asyncio
async def test_reconnect_with_same_peer_id_bypasses_hijack_guard(tmp_path):
    """The reconnect path (#167) uses a known peer_id; the hijack guard runs
    in the fresh-claim branch only and must not fire on legitimate reconnects."""
    registry = _make_registry(tmp_path)

    peer_id, name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/reconnect-proj",
        pane_id="%4",
        agent_pid=88888,
    )

    peer = await registry.get_peer(peer_id)
    assert peer is not None and peer.last_seen is not None
    before_reconnect = peer.last_seen
    peer.status = PeerStatus.OFFLINE
    peer.last_seen = before_reconnect - timedelta(seconds=30)

    reclaimed_id, reclaimed_name = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/reconnect-proj",
        pane_id="%4",
        peer_id=peer_id,
        agent_pid=88888,
        parent_pid=88888,  # would normally trip the guard if this were a fresh claim
    )
    assert reclaimed_id == peer_id
    assert reclaimed_name == name
    reconnected = await registry.get_peer(peer_id)
    assert reconnected is not None
    assert reconnected.status == PeerStatus.ONLINE
    assert reconnected.last_seen is not None
    assert reconnected.last_seen > before_reconnect - timedelta(seconds=30)


@pytest.mark.asyncio
async def test_hijack_guard_skipped_when_existing_agent_pid_missing(tmp_path):
    """Peers registered before this feature have no agent_pid recorded; the
    guard cannot decide and must not block them."""
    registry = _make_registry(tmp_path)

    await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path="/tmp/legacy-proj",
        pane_id="%5",
        # agent_pid intentionally omitted (legacy peer)
    )

    new_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.GEMINI,
        path="/tmp/sub",
        pane_id="%5",
        agent_pid=12121,
        parent_pid=99999,
    )
    peer = await registry.get_peer_by_pane("%5")
    assert peer is not None and peer.peer_id == new_id


@pytest.mark.asyncio
async def test_agent_pid_persists_in_session_mapping(tmp_path):
    """agent_pid is written to and read back from the on-disk SessionMapping.
    Persisting it is the prerequisite for the hijack guard surviving daemon
    restart: when the peer rehydrates via its ws-hook reconnect (same
    peer_id), the registry has agent_pid to compare against."""
    registry = _make_registry(tmp_path)

    original_agent_pid = 51515
    peer_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%9",
        agent_pid=original_agent_pid,
    )
    registry._mappings_dirty = True
    registry._persist_mappings()

    raw = json.loads((tmp_path / "sessions.json").read_text())
    assert raw[peer_id]["agent_pid"] == original_agent_pid

    reloaded = _make_registry(tmp_path)
    mapping = reloaded.get_mapping(peer_id)
    assert mapping is not None
    assert mapping.agent_pid == original_agent_pid


@pytest.mark.asyncio
async def test_reconnect_updates_agent_pid_on_live_peer_and_mapping(tmp_path):
    """When the same peer_id reconnects after the original agent process
    has changed (e.g. daemon-restart followed by ws-hook reconnect with a
    fresh agent_pid), both the live Peer and the SessionMapping are
    refreshed so subsequent hijack checks compare against the live pid."""
    registry = _make_registry(tmp_path)

    peer_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%r",
        agent_pid=10001,
    )
    m0 = registry.get_mapping(peer_id)
    assert m0 is not None and m0.agent_pid == 10001

    reclaimed_id, _ = await registry.allocate_and_register(
        circle="default",
        backend=AgentType.CLAUDE_CODE,
        path=str(tmp_path),
        pane_id="%r",
        peer_id=peer_id,
        agent_pid=20002,
    )
    assert reclaimed_id == peer_id
    peer = await registry.get_peer(peer_id)
    assert peer is not None
    assert peer.agent_pid == 20002
    m1 = registry.get_mapping(peer_id)
    assert m1 is not None and m1.agent_pid == 20002
