"""Durable autonomous job dispatch runner."""

from __future__ import annotations

import asyncio
import logging
from datetime import datetime, timedelta, timezone
from typing import Any

from fastapi import HTTPException

from repowire.config.models import AgentType, Config
from repowire.daemon.peer_delivery import PeerDeliveryService
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.spawn_service import SpawnService
from repowire.daemon.work_store import TrackedWork, now_iso
from repowire.protocol.peers import Peer, PeerStatus

logger = logging.getLogger(__name__)


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
        peer_registry: PeerRegistry,
        peer_delivery: PeerDeliveryService,
        spawn_service: SpawnService,
        runner_owner_id: str = "daemon-job-runner",
        lease_seconds: int = 300,
        poll_interval: float | None = None,
    ) -> None:
        self._config = config
        self._store = work_store
        self._registry = peer_registry
        self._delivery = peer_delivery
        self._spawn = spawn_service
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
        if soonest is None:
            return None
        return max(0.0, (soonest - now).total_seconds())

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
            assigned_peer_info={
                "peer_id": peer.peer_id,
                "display_name": peer.display_name,
                "backend": peer.backend.value,
                "path": peer.path,
                "status": peer.status.value,
            },
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
            )
            return resolved

        path = target.get("path")
        backend_raw = target.get("backend")
        if not path or not backend_raw:
            return self._mark_unavailable(work, attempt_id, "missing_target")
        try:
            backend = AgentType(str(backend_raw))
        except ValueError:
            return self._mark_unavailable(work, attempt_id, "invalid_backend")
        warmup = (
            "Repowire spawned this session for a durable job. "
            "Please register with the mesh; the job request will arrive as an ask."
        )
        try:
            spawn_result = self._spawn.spawn(
                path=str(path),
                backend=backend,
                profile=target.get("profile"),
                circle=work.circle or "default",
                message=warmup,
            )
        except HTTPException as e:
            return self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                status="failed",
                phase="spawn",
                error={"reason": "spawn_failed", "status_code": e.status_code, "detail": e.detail},
            )
        except Exception as e:
            return self._store.update_attempt(
                work.work_id,
                attempt_id=attempt_id,
                status="failed",
                phase="spawn",
                error={"reason": "spawn_failed", "message": str(e), "type": type(e).__name__},
            )
        self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            phase="spawned",
            tmux={"tmux_session": spawn_result.tmux_session, "pane_id": spawn_result.pane_id},
        )
        resolved = await self._await_spawned_peer(spawn_result.display_name, work.circle)
        if resolved is None:
            return self._mark_unavailable(work, attempt_id, "spawned_peer_not_registered")
        return resolved

    async def _await_spawned_peer(
        self,
        display_name: str,
        circle: str | None,
        *,
        timeout_seconds: float = 10.0,
    ) -> Peer | None:
        deadline = _utcnow() + timedelta(seconds=timeout_seconds)
        while True:
            resolved = await self._registry.resolve_peer_strict(display_name, circle=circle)
            if not isinstance(resolved, list):
                return resolved
            if _utcnow() >= deadline:
                return None
            await asyncio.sleep(0.25)

    def _mark_unavailable(
        self,
        work: TrackedWork,
        attempt_id: str,
        reason: str,
        *,
        assigned_peer_id: str | None = None,
    ) -> None:
        self._store.update_attempt(
            work.work_id,
            attempt_id=attempt_id,
            status="unavailable",
            phase="resolve_peer",
            assigned_peer_id=assigned_peer_id,
            error={"reason": reason},
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
