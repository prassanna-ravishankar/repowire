"""Fire-completion service: structural job results via Stop-hook chat turns."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from repowire.daemon.ask_tracker import AskTracker
from repowire.daemon.job_completion import JobCompletionService
from repowire.daemon.state.database import StateDatabase
from repowire.daemon.state.work import SQLiteWorkStore
from repowire.protocol.peers import PeerRole, PeerStatus

EXECUTOR = "repow-exec-1"


class FakeRegistry:
    def __init__(self, peers: list | None = None) -> None:
        self.events: list[tuple[str, dict]] = []
        self.peers = peers or []

    def add_event(self, event_type: str, data: dict) -> str:
        self.events.append((event_type, data))
        return f"evt-{len(self.events)}"

    async def get_all_peers(self):
        return self.peers

    async def get_peer(self, identifier: str, circle: str | None = None):
        for peer in self.peers:
            if peer.peer_id == identifier:
                return peer
        return None


class FakeSessionControl:
    def __init__(self) -> None:
        self.released: list[tuple[str, str]] = []

    async def release_executor_for_work(self, work, *, terminal_reason: str):
        self.released.append((work.work_id, terminal_reason))
        return {"status": "released", "released_at": "2026-01-01T00:00:00+00:00"}


class FakeDelivery:
    def __init__(self, deliver_to: set[str] | None = None) -> None:
        self.notifies: list[dict] = []
        self.deliver_to = deliver_to  # None -> deliver to everyone

    async def notify_result(self, *, from_peer, to_peer, text, **kwargs):
        self.notifies.append({"from": from_peer, "to": to_peer, "text": text})
        delivered = self.deliver_to is None or to_peer in self.deliver_to
        if not delivered:
            raise ValueError(f"unknown peer {to_peer}")
        return SimpleNamespace(delivered=True, queued=False)


def _store(tmp_path) -> tuple[StateDatabase, SQLiteWorkStore]:
    db = StateDatabase(tmp_path / "state.db")
    return db, SQLiteWorkStore(db)


def _delivered_work(store: SQLiteWorkStore, *, peer_id: str = EXECUTOR):
    work = store.create(title="nightly job", created_by_peer_id="repow-orch-1")
    acquired = store.acquire_for_dispatch(
        work.work_id,
        runner_owner_id="r1",
        lease_until="2099-01-01T00:00:00+00:00",
    )
    attempt_id = acquired.provenance["runner"]["current_attempt_id"]
    store.update_attempt(
        work.work_id,
        attempt_id=attempt_id,
        status="delivered",
        phase="delivered",
        assigned_peer_id=peer_id,
        correlation_id="ask-job1",
    )
    return store.get(work.work_id), attempt_id


def _prompt_marker(work, attempt_id: str) -> str:
    return (
        f"[ask #ask-job1 from @jobs] [Repowire durable job]\n"
        f"job_id: {work.work_id}\nattempt_id: {attempt_id}\n\ndo the thing"
    )


@pytest.mark.asyncio
async def test_user_marker_arms_fire(tmp_path):
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    svc = JobCompletionService(work_store=store)

    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )

    armed = store.get(work.work_id)
    assert armed.state == "running"
    assert armed.state_reason == "turn_started"
    assert armed.phase == "turn_started"
    db.close()


@pytest.mark.asyncio
async def test_marker_from_wrong_peer_does_not_arm(tmp_path):
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    svc = JobCompletionService(work_store=store)

    await svc.on_chat_turn(
        peer_id="repow-impostor", role="user", text=_prompt_marker(work, attempt_id),
    )

    assert store.get(work.work_id).state == "delivered"
    db.close()


@pytest.mark.asyncio
async def test_assistant_turn_completes_armed_fire(tmp_path):
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    ask_tracker = AskTracker()
    await ask_tracker.register(
        from_peer_id="repow-jobs", from_peer_name="jobs",
        to_peer_id=EXECUTOR, to_peer_name="exec",
        text="job", correlation_id="ask-job1",
    )
    session_control = FakeSessionControl()
    svc = JobCompletionService(
        work_store=store, ask_tracker=ask_tracker, session_control=session_control,
    )

    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="assistant", text="All done. Report: shipped.",
    )

    done = store.get(work.work_id)
    assert done.state == "completed"
    assert done.state_reason == "turn_complete"
    assert done.result_summary == "All done. Report: shipped."
    assert done.result_data["final_message"] == "All done. Report: shipped."
    ask = await ask_tracker.get("ask-job1")
    assert ask.closed and ask.close_reason == "fire_completed"
    db.close()


@pytest.mark.asyncio
async def test_assistant_turn_without_armed_work_is_noop(tmp_path):
    db, store = _store(tmp_path)
    work, _attempt_id = _delivered_work(store)
    svc = JobCompletionService(work_store=store)

    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="unrelated turn")

    assert store.get(work.work_id).state == "delivered"
    db.close()


@pytest.mark.asyncio
async def test_explicit_phase_change_holds_fire_open(tmp_path):
    """job_update with an explicit phase opts out of Stop-completion (the
    escape hatch for fires blocked on something outside the mesh)."""
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    svc = JobCompletionService(work_store=store)
    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    store.update_state(
        work.work_id, state="running", state_reason="waiting_ci", phase="waiting_ci",
    )

    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="waiting for CI")

    assert store.get(work.work_id).state == "running"
    db.close()


@pytest.mark.asyncio
async def test_executor_terminal_offline_fails_inflight_fire(tmp_path):
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    session_control = FakeSessionControl()
    svc = JobCompletionService(work_store=store, session_control=session_control)

    await svc.on_peer_terminal_offline(EXECUTOR, reason="agent_exited")

    failed = store.get(work.work_id)
    assert failed.state == "failed"
    assert failed.state_reason == "executor_died"
    assert failed.error["reason"] == "executor_died"
    assert failed.error["detail"] == "agent_exited"
    db.close()


@pytest.mark.asyncio
async def test_terminal_offline_of_unrelated_peer_is_noop(tmp_path):
    db, store = _store(tmp_path)
    work, _attempt_id = _delivered_work(store)
    svc = JobCompletionService(work_store=store)

    await svc.on_peer_terminal_offline("repow-other", reason="session_end")

    assert store.get(work.work_id).state == "delivered"
    db.close()


@pytest.mark.asyncio
async def test_state_changes_emit_job_state_changed_events(tmp_path):
    db, store = _store(tmp_path)
    registry = FakeRegistry()
    svc = JobCompletionService(work_store=store, peer_registry=registry)
    store.set_on_state_change(svc.on_work_state_change)

    work, attempt_id = _delivered_work(store)
    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="done")

    transitions = [
        (data["prior_state"], data["state"])
        for event_type, data in registry.events
        if event_type == "job_state_changed"
    ]
    assert ("queued", "dispatching") in transitions
    assert ("dispatching", "delivered") in transitions
    assert ("delivered", "running") in transitions
    assert ("running", "completed") in transitions
    db.close()


@pytest.mark.asyncio
async def test_terminal_state_notifies_creator(tmp_path):
    db, store = _store(tmp_path)
    registry = FakeRegistry()
    delivery = FakeDelivery()
    svc = JobCompletionService(
        work_store=store, peer_registry=registry, peer_delivery=delivery,
    )
    svc.set_sender_peer_id("repow-jobs")
    store.set_on_state_change(svc.on_work_state_change)

    work, attempt_id = _delivered_work(store)
    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="shipped")
    # _finalize runs as a task scheduled from the sync callback
    for task in list(svc._finalize_tasks):
        await task

    assert delivery.notifies, "terminal state should notify the creator"
    first = delivery.notifies[0]
    assert first["to"] == "repow-orch-1"
    assert first["from"] == "repow-jobs"
    assert work.work_id in first["text"]
    assert "completed" in first["text"]
    db.close()


@pytest.mark.asyncio
async def test_notify_falls_back_to_orchestrator_when_creator_gone(tmp_path):
    db, store = _store(tmp_path)
    orch = SimpleNamespace(
        peer_id="repow-orch-live",
        role=PeerRole.ORCHESTRATOR,
        status=PeerStatus.ONLINE,
        circle=None,
    )
    registry = FakeRegistry(peers=[orch])
    delivery = FakeDelivery(deliver_to={"repow-orch-live"})
    svc = JobCompletionService(
        work_store=store, peer_registry=registry, peer_delivery=delivery,
    )
    store.set_on_state_change(svc.on_work_state_change)

    work, attempt_id = _delivered_work(store)
    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="shipped")
    for task in list(svc._finalize_tasks):
        await task

    assert [n["to"] for n in delivery.notifies] == ["repow-orch-1", "repow-orch-live"]
    db.close()


@pytest.mark.asyncio
async def test_completion_releases_executor(tmp_path):
    db, store = _store(tmp_path)
    work, attempt_id = _delivered_work(store)
    store.update_attempt(
        work.work_id,
        attempt_id=attempt_id,
        acquisition={
            "release_handle": {
                "kind": "tmux_pane", "peer_id": EXECUTOR, "pane_id": "%9",
            },
        },
    )
    session_control = FakeSessionControl()
    svc = JobCompletionService(work_store=store, session_control=session_control)

    await svc.on_chat_turn(
        peer_id=EXECUTOR, role="user", text=_prompt_marker(work, attempt_id),
    )
    await svc.on_chat_turn(peer_id=EXECUTOR, role="assistant", text="done")

    assert session_control.released == [(work.work_id, "completed")]
    db.close()


@pytest.mark.asyncio
async def test_restart_reconcile_fails_fires_with_dead_executor(tmp_path):
    db, store = _store(tmp_path)
    registry = FakeRegistry(peers=[])  # executor absent from roster
    svc = JobCompletionService(work_store=store, peer_registry=registry)
    work, _attempt_id = _delivered_work(store)

    await svc.reconcile_inflight(grace_seconds=0)

    failed = store.get(work.work_id)
    assert failed.state == "failed"
    assert failed.state_reason == "executor_died"
    assert failed.error["detail"] == "daemon_restart_reconcile"
    db.close()


@pytest.mark.asyncio
async def test_restart_reconcile_spares_live_executor(tmp_path):
    db, store = _store(tmp_path)
    live = SimpleNamespace(
        peer_id=EXECUTOR,
        role=PeerRole.AGENT,
        status=PeerStatus.BUSY,
        circle="default",
    )
    registry = FakeRegistry(peers=[live])
    svc = JobCompletionService(work_store=store, peer_registry=registry)
    work, _attempt_id = _delivered_work(store)

    await svc.reconcile_inflight(grace_seconds=0)

    assert store.get(work.work_id).state == "delivered"
    db.close()
