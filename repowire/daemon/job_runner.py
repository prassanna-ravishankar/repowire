"""Durable autonomous job dispatch runner."""

from __future__ import annotations

import asyncio
import logging
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from fastapi import HTTPException

from repowire.agent_backends import AgentResumePlan
from repowire.agent_types import AgentType
from repowire.config.models import Config
from repowire.daemon.peer_delivery import PeerDeliveryService
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.resume_safety import resolve_resume_safety
from repowire.daemon.session_control import (
    ExecutorAcquisitionUnavailableError,
    SessionControlService,
)
from repowire.daemon.spawn_service import SpawnAttemptError, SpawnService
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.work_store import TrackedWork, now_iso
from repowire.protocol.peers import Peer, PeerStatus

logger = logging.getLogger(__name__)

SPAWN_REGISTRATION_TIMEOUT_SECONDS = 45.0


def _parse_due(value: str | None) -> datetime | None:
    if not value:
        return None
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


class JobRunner:
    """Acquires queued jobs and dispatches them exactly once per attempt."""

    def __init__(
        self,
        *,
        config: Config,
        work_store: Any,
        calendar_store: Any | None = None,
        peer_registry: PeerRegistry,
        peer_delivery: PeerDeliveryService,
        spawn_service: SpawnService,
        session_binding_store: SQLiteSessionBindingStore | None = None,
        session_control: SessionControlService | None = None,
        runner_owner_id: str = "daemon-job-runner",
        lease_seconds: int = 300,
        poll_interval: float | None = None,
    ) -> None:
        self._config = config
        self._store = work_store
        self._calendar = calendar_store
        self._registry = peer_registry
        self._delivery = peer_delivery
        self._spawn = spawn_service
        self._session_bindings = session_binding_store
        self._session_control = session_control
        self._runner_owner_id = runner_owner_id
        self._lease_seconds = lease_seconds
        self._task: asyncio.Task | None = None
        self._stopped = asyncio.Event()
        self._wake = asyncio.Event()

    async def start(self) -> None:
        self.recover_stale()
        self._stopped.clear()
        self._wake.clear()
        self._task = asyncio.create_task(self._loop())

    def wake(self) -> None:
        """Wake the runner after job create/update instead of polling."""
        self._wake.set()

    async def stop(self) -> None:
        self._stopped.set()
        if self._task is None:
            return
        self._task.cancel()
        try:
            await self._task
        except asyncio.CancelledError:
            pass
        self._task = None

    def recover_stale(self) -> list[TrackedWork]:
        return self._store.recover_stale_dispatching(
            now=now_iso(), runner_owner_id=self._runner_owner_id
        )

    async def _loop(self) -> None:
        try:
            while not self._stopped.is_set():
                # Clear before inspecting state so a create/update that races
                # with deadline computation remains set and wakes the wait.
                self._wake.clear()
                try:
                    self.recover_stale()
                    self.materialize_due_calendar()
                    await self.run_due_once()
                except asyncio.CancelledError:
                    raise
                except Exception:
                    logger.exception("job runner tick failed")
                delay = self._seconds_until_next_deadline()
                waiters = [
                    asyncio.create_task(self._stopped.wait()),
                    asyncio.create_task(self._wake.wait()),
                ]
                try:
                    done, pending = await asyncio.wait(
                        waiters,
                        timeout=delay,
                        return_when=asyncio.FIRST_COMPLETED,
                    )
                    for task in pending:
                        task.cancel()
                    for task in done:
                        task.result()
                finally:
                    for task in waiters:
                        if not task.done():
                            task.cancel()
        except asyncio.CancelledError:
            raise

    def _seconds_until_next_deadline(self) -> float | None:
        """Return seconds until next concrete due/lease deadline, or None.

        None means there is no known future job deadline, so the runner sleeps
        indefinitely until create/update wakes it or the daemon stops.
        """
        soonest: datetime | None = None
        now = _utcnow()
        for work in self._store.list_all(state="queued"):
            execution = (work.request or {}).get("execution") or {}
            due_at = (execution.get("schedule") or {}).get("due_at")
            due = _parse_due(due_at) or now
            if soonest is None or due < soonest:
                soonest = due
        for state in ("dispatching", "delivered"):
            for work in self._store.list_all(state=state):
                runner = (work.provenance or {}).get("runner") or {}
                lease_until = _parse_due(runner.get("lease_until"))
                if lease_until is not None and (soonest is None or lease_until < soonest):
                    soonest = lease_until
        if self._calendar is not None:
            calendar_delay = self._calendar.seconds_until_next_due(now=now)
            if calendar_delay is not None:
                calendar_due = now + timedelta(seconds=calendar_delay)
                if soonest is None or calendar_due < soonest:
                    soonest = calendar_due
        if soonest is None:
            return None
        return max(0.0, (soonest - now).total_seconds())

    def materialize_due_calendar(self) -> list[TrackedWork]:
        if self._calendar is None:
            return []
        materialized = self._calendar.materialize_due(now=_utcnow())
        if materialized:
            self._wake.set()
        return materialized

    async def run_due_once(self) -> list[str]:
        dispatched: list[str] = []
        for work in self._store.list_all(state="queued"):
            execution = (work.request or {}).get("execution") or {}
            due_at = (execution.get("schedule") or {}).get("due_at")
            due = _parse_due(due_at)
            if due is not None and due > _utcnow():
                continue
            result = await self.run_job(work.work_id, ignore_due_at=False)
            if result is not None:
                dispatched.append(work.work_id)
        return dispatched

    async def run_job(
        self, work_id: str, *, ignore_due_at: bool = True, retry: bool = False
    ) -> TrackedWork | None:
        lease_until = (_utcnow() + timedelta(seconds=self._lease_seconds)).isoformat()
        acquired = self._store.acquire_for_dispatch(
            work_id,
            runner_owner_id=self._runner_owner_id,
            lease_until=lease_until,
            ignore_due_at=ignore_due_at,
            retry=retry,
        )
        if acquired is None:
            return None
        attempt_id = (acquired.provenance.get("runner") or {}).get("current_attempt_id")
        if not attempt_id:
            return acquired
        try:
            return await self._dispatch(acquired, attempt_id)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # keep dispatch audit-visible
            return self._store.update_attempt(
                work_id,
                attempt_id=attempt_id,
                status="failed",
                phase="dispatch",
                error={"reason": "dispatch_failed", "message": str(e), "type": type(e).__name__},
            )

    async def _dispatch(self, work: TrackedWork, attempt_id: str) -> TrackedWork | None:
        current = self._store.get(work.work_id)
        if current is None:
            return None
        if current.cancel_requested:
            return self._store.update_attempt(
                work.work_id, attempt_id=attempt_id, status="cancelled", phase="cancelled"
            )

        peer = await self._resolve_or_spawn_peer(current, attempt_id)
        if peer is None:
            return self._store.get(work.work_id)

        current = self._store.get(work.work_id)
        if current is None:
            return None
        if current.cancel_requested:
            return self._store.update_attempt(
                work.work_id, attempt_id=attempt_id, status="cancelled", phase="cancelled"
            )

        text = self._build_prompt(current, attempt_id)
        self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            phase="delivery",
            assigned_peer_id=peer.peer_id,
            assigned_peer_info=self._peer_info(peer, work=work),
            tmux={"tmux_session": peer.tmux_session, "pane_id": peer.pane_id},
            delivery_state="pending",
        )
        try:
            cid = await self._delivery.open_scheduled_ask(
                from_peer=self._runner_owner_id,
                to_peer=peer.peer_id,
                text=text,
                circle=peer.circle,
            )
        except Exception as e:
            return self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                status="failed",
                phase="delivery",
                delivery_state="failed",
                error={
                    "reason": "ask_delivery_failed",
                    "message": str(e),
                    "type": type(e).__name__,
                },
            )
        return self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            status="delivered",
            phase="delivered",
            delivery_state="delivered",
            correlation_id=cid,
        )

    async def _resolve_or_spawn_peer(self, work: TrackedWork, attempt_id: str) -> Peer | None:
        execution = (work.request or {}).get("execution") or {}
        target = execution.get("target") or {}
        if self._session_control is not None:
            try:
                acquisition = await self._session_control.acquire_executor_for_work(
                    work,
                    target=target,
                    runner_owner_id=self._runner_owner_id,
                )
            except ExecutorAcquisitionUnavailableError as e:
                self._store.update_attempt(
                    work.work_id,
                    attempt_id=attempt_id,
                    status=e.status,
                    phase=e.phase,
                    assigned_peer_id=e.assigned_peer_id,
                    operation_id=e.operation_id,
                    error=e.error,
                )
                return None
            peer = acquisition.peer
            phase_by_strategy = {
                "assigned_peer": "resolved_peer",
                "reused_peer": "reused_peer",
                "backend_resume": "resumed_peer_registered",
                "spawned_peer": "spawned_peer_registered",
            }
            self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                phase=phase_by_strategy.get(acquisition.strategy, "resolved_peer"),
                assigned_peer_id=peer.peer_id,
                assigned_peer_info=self._peer_info(peer, work=work),
                tmux={"tmux_session": peer.tmux_session, "pane_id": peer.pane_id},
                resume_plan=self._resume_plan_info(acquisition.resume_plan),
                operation_id=acquisition.operation_id,
                acquisition_strategy=acquisition.strategy,
                acquisition={
                    "operation_id": acquisition.operation_id,
                    "strategy": acquisition.strategy,
                    "runtime_binding": acquisition.runtime_binding,
                    "release_handle": acquisition.release_handle,
                },
            )
            return peer
        assigned = target.get("assigned_peer_id") or work.assigned_peer_id
        if assigned:
            resolved = await self._registry.resolve_peer_strict(str(assigned), circle=work.circle)
            if isinstance(resolved, list):
                return self._mark_unavailable(
                    work,
                    attempt_id,
                    "assigned_peer_not_found" if not resolved else "ambiguous_assigned_peer",
                )
            if resolved.status == PeerStatus.OFFLINE:
                return self._mark_unavailable(
                    work, attempt_id, "assigned_peer_offline", assigned_peer_id=resolved.peer_id
                )
            self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                phase="resolved_peer",
                assigned_peer_id=resolved.peer_id,
                assigned_peer_info=self._peer_info(resolved, work=work),
                tmux={"tmux_session": resolved.tmux_session, "pane_id": resolved.pane_id},
            )
            self._record_runtime_binding(work, resolved, source="assigned_peer")
            return resolved

        path = target.get("path")
        backend_raw = target.get("backend")
        if not path or not backend_raw:
            return self._mark_unavailable(work, attempt_id, "missing_target")
        try:
            backend = AgentType(str(backend_raw))
        except ValueError:
            return self._mark_unavailable(work, attempt_id, "invalid_backend")
        reusable = await self._find_reusable_peer(
            path=str(path),
            backend=backend,
            circle=work.circle or "default",
        )
        if reusable is not None:
            self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                phase="reused_peer",
                assigned_peer_id=reusable.peer_id,
                assigned_peer_info=self._peer_info(reusable, work=work),
                tmux={"tmux_session": reusable.tmux_session, "pane_id": reusable.pane_id},
            )
            self._record_runtime_binding(work, reusable, source="reused_peer")
            return reusable
        warmup = (
            "Repowire spawned this session for a durable job. "
            "Please register with the mesh; the job request will arrive as an ask."
        )
        resume_plan = self._resume_plan_for(work, path=str(path), backend=backend)
        def mark_spawned(spawn_result) -> None:
            self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                phase="resume_spawned" if resume_plan is not None else "spawned",
                tmux={"tmux_session": spawn_result.tmux_session, "pane_id": spawn_result.pane_id},
                resume_plan=self._resume_plan_info(resume_plan),
            )

        try:
            registration = await self._spawn.spawn_registered(
                path=str(path),
                backend=backend,
                profile=target.get("profile"),
                circle=work.circle or "default",
                message=warmup,
                resume_plan=resume_plan,
                on_spawned=mark_spawned,
                await_peer=lambda spawn_result: self._await_spawned_peer(
                    spawn_result.display_name,
                    work.circle or "default",
                    path=str(path),
                    backend=backend,
                    pane_id=spawn_result.pane_id,
                ),
            )
        except SpawnAttemptError as e:
            original = e.original
            if isinstance(original, HTTPException):
                error = {
                    "reason": "spawn_failed",
                    "status_code": original.status_code,
                    "detail": original.detail,
                    "attempt": e.attempt,
                    "registration_attempts": e.registration_attempts,
                }
            else:
                error = {
                    "reason": "spawn_failed",
                    "message": str(original),
                    "type": type(original).__name__,
                    "attempt": e.attempt,
                    "registration_attempts": e.registration_attempts,
                }
            return self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                status="failed",
                phase="spawn",
                error=error,
            )

        resolved = registration.resolved_peer
        if resolved is None:
            return self._mark_unavailable(
                work,
                attempt_id,
                "spawned_peer_not_registered",
                error={
                    "reason": "spawned_peer_not_registered",
                    "registration_attempts": registration.registration_attempts,
                },
            )
        self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            phase="resumed_peer_registered"
            if resume_plan is not None
            else "spawned_peer_registered",
            assigned_peer_id=resolved.peer_id,
            assigned_peer_info=self._peer_info(resolved, work=work),
            tmux={"tmux_session": resolved.tmux_session, "pane_id": resolved.pane_id},
        )
        self._record_runtime_binding(
            work,
            resolved,
            source="backend_resume" if resume_plan is not None else "spawned_peer",
        )
        return resolved

    async def _find_reusable_peer(
        self,
        *,
        path: str,
        backend: AgentType,
        circle: str,
    ) -> Peer | None:
        target_path = self._normalize_path(path)
        candidates: list[Peer] = []
        for peer in await self._registry.get_all_peers():
            if peer.status not in (PeerStatus.ONLINE, PeerStatus.BUSY):
                continue
            if peer.circle != circle or peer.backend != backend:
                continue
            if self._normalize_path(peer.path) != target_path:
                continue
            candidates.append(peer)
        if not candidates:
            return None
        return max(
            candidates,
            key=lambda peer: (
                peer.status == PeerStatus.ONLINE,
                peer.last_seen or datetime.min.replace(tzinfo=timezone.utc),
            ),
        )

    async def _await_spawned_peer(
        self,
        display_name: str,
        circle: str | None,
        *,
        path: str,
        backend: AgentType,
        pane_id: str | None,
        timeout_seconds: float = SPAWN_REGISTRATION_TIMEOUT_SECONDS,
    ) -> Peer | None:
        target_path = self._normalize_path(path)
        deadline = _utcnow() + timedelta(seconds=timeout_seconds)
        while True:
            if pane_id:
                pane_peer = await self._registry.get_peer_by_pane(pane_id)
                if pane_peer is not None and pane_peer.status != PeerStatus.OFFLINE:
                    return pane_peer
            resolved = await self._registry.resolve_peer_strict(display_name, circle=circle)
            if not isinstance(resolved, list) and resolved.status != PeerStatus.OFFLINE:
                return resolved
            path_peer = await self._find_registered_spawned_peer(
                path=target_path,
                backend=backend,
                circle=circle or "default",
            )
            if path_peer is not None:
                return path_peer
            if _utcnow() >= deadline:
                return None
            await asyncio.sleep(0.25)

    async def _find_registered_spawned_peer(
        self,
        *,
        path: str,
        backend: AgentType,
        circle: str,
    ) -> Peer | None:
        candidates: list[Peer] = []
        for peer in await self._registry.get_all_peers():
            if peer.status not in (PeerStatus.ONLINE, PeerStatus.BUSY):
                continue
            if peer.circle != circle or peer.backend != backend:
                continue
            if self._normalize_path(peer.path) != path:
                continue
            candidates.append(peer)
        if not candidates:
            return None
        return max(
            candidates,
            key=lambda peer: (
                bool(peer.pane_id),
                peer.last_seen or datetime.min.replace(tzinfo=timezone.utc),
            ),
        )

    def _resume_plan_for(
        self,
        work: TrackedWork,
        *,
        path: str,
        backend: AgentType,
    ) -> AgentResumePlan | None:
        if self._calendar is None or work.source_kind != "calendar" or not work.source_id:
            return None
        entry = self._calendar.get(work.source_id)
        if entry is None:
            return None
        binding = (entry.provenance or {}).get("runtime_binding") or {}
        if not isinstance(binding, dict):
            return None
        if binding.get("backend") != backend.value:
            return None
        if self._normalize_path(binding.get("path")) != self._normalize_path(path):
            return None
        circle = work.circle or "default"
        if binding.get("circle") and binding.get("circle") != circle:
            return None
        runtime_session_id = binding.get("runtime_session_id")
        if not isinstance(runtime_session_id, str) or not runtime_session_id:
            return None
        capability = binding.get("resume_capability") or {}
        repowire_session_id = binding.get("repowire_session_id")
        # Shared resume-safety seam (same as restart + session_control): refuse
        # to attach a plan unless the id is actually pre-validated on disk, so a
        # stale id makes the executor start fresh instead of failing hard.
        decision = resolve_resume_safety(
            backend=backend,
            path=path,
            runtime_session_id=runtime_session_id,
            repowire_session_id=(
                repowire_session_id if isinstance(repowire_session_id, str) else None
            ),
            capability=capability if isinstance(capability, dict) else None,
        )
        if not decision.resumable and decision.warning:
            logger.info(
                "Job %s: not resuming backend session (%s)", work.work_id, decision.warning
            )
        return decision.plan

    @staticmethod
    def _resume_plan_info(plan: AgentResumePlan | None) -> dict[str, Any] | None:
        if plan is None:
            return None
        return {
            "backend": plan.backend.value,
            "runtime_session_id": plan.runtime_session_id,
            "repowire_session_id": plan.repowire_session_id,
            "capability": plan.capability or {},
        }

    def _record_runtime_binding(
        self,
        work: TrackedWork,
        peer: Peer,
        *,
        source: str,
    ) -> None:
        binding = self._runtime_binding_for_peer(peer, work=work)
        if not binding:
            return
        binding["source"] = source
        binding["recorded_at"] = now_iso()
        if self._calendar is not None and work.source_kind == "calendar" and work.source_id:
            self._calendar.update_runtime_binding(work.source_id, binding=binding)

    def _runtime_binding_for_peer(
        self,
        peer: Peer,
        *,
        work: TrackedWork | None = None,
    ) -> dict[str, Any]:
        runtime_session_id = self._runtime_session_id_for_peer(peer)
        session_binding = None
        if self._session_bindings is not None:
            if runtime_session_id:
                session_binding = self._session_bindings.get_by_runtime_session(
                    runtime_session_id,
                    backend=peer.backend,
                    project_path=peer.path,
                )
            if session_binding is None:
                bindings = self._session_bindings.list_by_peer(peer.peer_id)
                if bindings:
                    session_binding = bindings[0]
        binding: dict[str, Any] = {
            "peer_id": peer.peer_id,
            "display_name": peer.display_name,
            "backend": peer.backend.value,
            "path": peer.path,
            "circle": peer.circle,
            "status": peer.status.value,
            "tmux": {"tmux_session": peer.tmux_session, "pane_id": peer.pane_id},
        }
        if work is not None:
            binding["work_id"] = work.work_id
        if runtime_session_id:
            binding["runtime_session_id"] = runtime_session_id
        if peer.metadata:
            binding["metadata"] = dict(peer.metadata)
        if session_binding is not None:
            binding.update(
                {
                    "repowire_session_id": session_binding.repowire_session_id,
                    "runtime_session_id": session_binding.runtime_session_id
                    or binding.get("runtime_session_id"),
                    "runtime_source_uri": session_binding.runtime_source_uri,
                    "source_cursor": session_binding.source_cursor,
                    "resume_capability": session_binding.resume_capability,
                    "binding_status": session_binding.status,
                }
            )
        return binding

    @staticmethod
    def _runtime_session_id_for_peer(peer: Peer) -> str | None:
        metadata = peer.metadata or {}
        for key in ("runtime_session_id", "hook_session_id", "session_id"):
            value = metadata.get(key)
            if isinstance(value, str) and value:
                return value
        return None

    @staticmethod
    def _normalize_path(path: str | None) -> str:
        if not path:
            return ""
        return str(Path(path).expanduser().resolve())

    def _peer_info(self, peer: Peer, *, work: TrackedWork | None = None) -> dict[str, Any]:
        info: dict[str, Any] = {
            "peer_id": peer.peer_id,
            "display_name": peer.display_name,
            "backend": peer.backend.value,
            "path": peer.path,
            "status": peer.status.value,
        }
        if peer.metadata:
            # Preserve runtime-provided session handles for future resume support.
            info["metadata"] = dict(peer.metadata)
        runtime_binding = self._runtime_binding_for_peer(peer, work=work)
        if runtime_binding:
            info["runtime_binding"] = runtime_binding
        return info

    def _mark_unavailable(
        self,
        work: TrackedWork,
        attempt_id: str,
        reason: str,
        *,
        assigned_peer_id: str | None = None,
        error: dict[str, Any] | None = None,
    ) -> None:
        self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            status="unavailable",
            phase="resolve_peer",
            assigned_peer_id=assigned_peer_id,
            error=error or {"reason": reason},
        )
        return None

    @staticmethod
    def _build_prompt(work: TrackedWork, attempt_id: str) -> str:
        execution = (work.request or {}).get("execution") or {}
        prompt = execution.get("prompt") or {}
        body = prompt.get("body") or work.title
        return (
            f"[Repowire durable job]\n"
            f"job_id: {work.work_id}\n"
            f"attempt_id: {attempt_id}\n\n"
            f"{body}\n\n"
            "First, immediately PATCH /jobs/{job_id} to state=running with this "
            "attempt_id before doing longer work. When complete, PATCH /jobs/{job_id} "
            "with a terminal state and the same attempt_id. Ack only confirms receipt."
        )
