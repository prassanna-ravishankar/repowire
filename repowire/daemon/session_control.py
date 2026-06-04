"""Session-first executor acquisition for jobs and future controls."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from fastapi import HTTPException

from repowire.agent_backends import AgentResumePlan
from repowire.config.models import AgentType
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.resume_safety import resolve_resume_safety
from repowire.daemon.spawn_service import SpawnAttemptError, SpawnService
from repowire.daemon.state.operations import SQLiteOperationStore
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.work_store import TrackedWork, now_iso
from repowire.protocol.peers import Peer, PeerStatus
from repowire.spawn import kill_pane
from repowire.spawn_ownership import forget_spawn_ownership, probe_tmux_pane

SPAWN_REGISTRATION_TIMEOUT_SECONDS = 45.0


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


@dataclass(frozen=True)
class ExecutorAcquisition:
    peer: Peer
    strategy: str
    operation_id: str
    resume_plan: AgentResumePlan | None = None
    runtime_binding: dict[str, Any] | None = None
    release_handle: dict[str, Any] | None = None


class ExecutorAcquisitionUnavailableError(Exception):
    def __init__(
        self,
        reason: str,
        *,
        operation_id: str,
        status: str = "unavailable",
        phase: str = "resolve_peer",
        error: dict[str, Any] | None = None,
        assigned_peer_id: str | None = None,
    ) -> None:
        super().__init__(reason)
        self.reason = reason
        self.operation_id = operation_id
        self.status = status
        self.phase = phase
        self.error = error or {"reason": reason}
        self.assigned_peer_id = assigned_peer_id


class SessionControlService:
    """Acquire a live executor for durable session/job work."""

    def __init__(
        self,
        *,
        peer_registry: PeerRegistry,
        spawn_service: SpawnService,
        operation_store: SQLiteOperationStore,
        session_binding_store: SQLiteSessionBindingStore | None = None,
        calendar_store: Any | None = None,
    ) -> None:
        self._registry = peer_registry
        self._spawn = spawn_service
        self._operations = operation_store
        self._session_bindings = session_binding_store
        self._calendar = calendar_store

    async def acquire_executor_for_work(
        self,
        work: TrackedWork,
        *,
        target: dict[str, Any],
        runner_owner_id: str,
    ) -> ExecutorAcquisition:
        execution_policy = self._execution_policy(work)
        operation = self._operations.create(
            kind="session.acquire_executor",
            target={
                "work_id": work.work_id,
                "repowire_session_id": work.repowire_session_id,
                "source_kind": work.source_kind,
                "source_id": work.source_id,
                "circle": work.circle,
                "process_scope": execution_policy["process_scope"],
                "continuity": execution_policy["continuity"],
                "target": target,
            },
            provenance={"requested_by": runner_owner_id},
        )
        assigned = target.get("assigned_peer_id") or work.assigned_peer_id
        if assigned:
            return await self._acquire_assigned_peer(
                work,
                operation.operation_id,
                str(assigned),
                process_scope=execution_policy["process_scope"],
            )

        path = target.get("path")
        backend_raw = target.get("backend")
        if not path or not backend_raw:
            self._operations.fail(
                operation.operation_id,
                state="unavailable",
                error={"reason": "missing_target"},
            )
            raise ExecutorAcquisitionUnavailableError(
                "missing_target",
                operation_id=operation.operation_id,
            )
        try:
            backend = AgentType(str(backend_raw))
        except ValueError:
            self._operations.fail(
                operation.operation_id,
                state="unavailable",
                error={"reason": "invalid_backend"},
            )
            raise ExecutorAcquisitionUnavailableError(
                "invalid_backend",
                operation_id=operation.operation_id,
            )

        circle = work.circle or "default"
        if execution_policy["process_scope"] != "per_fire":
            reusable = await self._find_live_peer(path=str(path), backend=backend, circle=circle)
        else:
            reusable = None
        if reusable is not None:
            runtime_binding = self._record_runtime_binding(
                work, reusable, source="reused_peer"
            )
            self._operations.start_attempt(
                operation.operation_id,
                strategy="reused_peer",
                detail={"peer_id": reusable.peer_id},
            )
            self._operations.complete(
                operation.operation_id,
                strategy="reused_peer",
                result={
                    "peer_id": reusable.peer_id,
                    "runtime_binding": runtime_binding,
                },
            )
            return ExecutorAcquisition(
                peer=reusable,
                strategy="reused_peer",
                operation_id=operation.operation_id,
                runtime_binding=runtime_binding,
            )

        resume_plan = None
        if execution_policy["continuity"] == "resume":
            resume_plan = self._resume_plan_for(work, path=str(path), backend=backend)
        strategy = "backend_resume" if resume_plan is not None else "spawned_peer"
        self._operations.start_attempt(
            operation.operation_id,
            strategy=strategy,
            detail={
                "path": str(path),
                "backend": backend.value,
                "resume_plan": self.resume_plan_info(resume_plan),
            },
        )
        warmup = (
            "Repowire spawned this session for a durable job. "
            "Please register with the mesh; the job request will arrive as an ask."
        )
        try:
            registration = await self._spawn.spawn_registered(
                path=str(path),
                backend=backend,
                profile=target.get("profile"),
                circle=circle,
                message=warmup,
                resume_plan=resume_plan,
                await_peer=lambda spawn_result: self._await_spawned_peer(
                    spawn_result.display_name,
                    circle,
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
            self._operations.fail(
                operation.operation_id,
                strategy=strategy,
                error=error,
            )
            raise ExecutorAcquisitionUnavailableError(
                "spawn_failed",
                operation_id=operation.operation_id,
                status="failed",
                phase="spawn",
                error=error,
            ) from e

        resolved = registration.resolved_peer
        if resolved is None:
            error = {
                "reason": "spawned_peer_not_registered",
                "registration_attempts": registration.registration_attempts,
            }
            self._operations.fail(
                operation.operation_id,
                state="unavailable",
                strategy=strategy,
                error=error,
            )
            raise ExecutorAcquisitionUnavailableError(
                "spawned_peer_not_registered",
                operation_id=operation.operation_id,
                error=error,
            )
        runtime_binding = self._record_runtime_binding(
            work,
            resolved,
            source="backend_resume" if resume_plan is not None else "spawned_peer",
            operation_id=operation.operation_id,
            process_scope=execution_policy["process_scope"],
            continuity=execution_policy["continuity"],
        )
        release_handle = self._release_handle_for_peer(
            resolved,
            process_scope=execution_policy["process_scope"],
            operation_id=operation.operation_id,
            strategy=strategy,
        )
        if execution_policy["process_scope"] == "per_fire" and release_handle is None:
            error = {"reason": "release_handle_unavailable", "peer_id": resolved.peer_id}
            self._operations.fail(
                operation.operation_id,
                state="unavailable",
                strategy=strategy,
                error=error,
            )
            raise ExecutorAcquisitionUnavailableError(
                "release_handle_unavailable",
                operation_id=operation.operation_id,
                error=error,
            )
        self._operations.complete(
            operation.operation_id,
            strategy=strategy,
            result={
                "peer_id": resolved.peer_id,
                "tmux": {
                    "tmux_session": resolved.tmux_session,
                    "pane_id": resolved.pane_id,
                },
                "runtime_binding": runtime_binding,
                "release_handle": release_handle,
            },
        )
        return ExecutorAcquisition(
            peer=resolved,
            strategy=strategy,
            operation_id=operation.operation_id,
            resume_plan=resume_plan,
            runtime_binding=runtime_binding,
            release_handle=release_handle,
        )

    async def _acquire_assigned_peer(
        self,
        work: TrackedWork,
        operation_id: str,
        assigned: str,
        *,
        process_scope: str,
    ) -> ExecutorAcquisition:
        resolved = await self._registry.resolve_peer_strict(assigned, circle=work.circle)
        if isinstance(resolved, list):
            reason = "assigned_peer_not_found" if not resolved else "ambiguous_assigned_peer"
            self._operations.fail(operation_id, state="unavailable", error={"reason": reason})
            raise ExecutorAcquisitionUnavailableError(reason, operation_id=operation_id)
        if resolved.status == PeerStatus.OFFLINE:
            self._operations.fail(
                operation_id,
                state="unavailable",
                error={"reason": "assigned_peer_offline"},
            )
            raise ExecutorAcquisitionUnavailableError(
                "assigned_peer_offline",
                operation_id=operation_id,
                assigned_peer_id=resolved.peer_id,
            )
        runtime_binding = self._record_runtime_binding(work, resolved, source="assigned_peer")
        release_handle = self._release_handle_for_peer(
            resolved,
            process_scope=process_scope,
            operation_id=operation_id,
            strategy="assigned_peer",
        )
        if process_scope == "per_fire" and release_handle is None:
            error = {"reason": "release_handle_unavailable", "peer_id": resolved.peer_id}
            self._operations.fail(operation_id, state="unavailable", error=error)
            raise ExecutorAcquisitionUnavailableError(
                "release_handle_unavailable",
                operation_id=operation_id,
                assigned_peer_id=resolved.peer_id,
                error=error,
            )
        self._operations.start_attempt(
            operation_id,
            strategy="assigned_peer",
            detail={"peer_id": resolved.peer_id, "release_handle": release_handle},
        )
        self._operations.complete(
            operation_id,
            strategy="assigned_peer",
            result={
                "peer_id": resolved.peer_id,
                "runtime_binding": runtime_binding,
                "release_handle": release_handle,
            },
        )
        return ExecutorAcquisition(
            peer=resolved,
            strategy="assigned_peer",
            operation_id=operation_id,
            runtime_binding=runtime_binding,
            release_handle=release_handle,
        )

    async def release_executor_for_work(
        self,
        work: TrackedWork,
        *,
        terminal_reason: str,
    ) -> dict[str, Any]:
        """Release the executor acquired for the current job attempt.

        Release is idempotent: a missing or already-dead pane is recorded as an
        already-released result, while a live mismatched peer fails closed.
        """

        runner = (work.provenance or {}).get("runner") or {}
        attempt_id = runner.get("current_attempt_id")
        attempts = list(runner.get("attempts") or [])
        attempt = next(
            (
                item for item in attempts
                if isinstance(item, dict) and item.get("attempt_id") == attempt_id
            ),
            None,
        )
        acquisition = (attempt or {}).get("acquisition") or {}
        release_handle = acquisition.get("release_handle")
        if not isinstance(release_handle, dict):
            return {
                "status": "skipped",
                "reason": "no_release_handle",
                "terminal_reason": terminal_reason,
            }
        if release_handle.get("released_at"):
            return {
                **release_handle,
                "status": "already_released",
                "terminal_reason": terminal_reason,
            }

        operation = self._operations.create(
            kind="session.release_executor",
            target={
                "work_id": work.work_id,
                "attempt_id": attempt_id,
                "terminal_reason": terminal_reason,
                "release_handle": release_handle,
            },
            provenance={"acquire_operation_id": release_handle.get("operation_id")},
        )
        self._operations.start_attempt(
            operation.operation_id,
            strategy="kill_pane",
            detail={"release_handle": release_handle},
        )

        pane_id = release_handle.get("pane_id")
        peer_id = release_handle.get("peer_id")
        if not isinstance(pane_id, str) or not pane_id:
            error = {"reason": "missing_pane_id", "peer_id": peer_id}
            self._operations.fail(operation.operation_id, strategy="kill_pane", error=error)
            return {
                "status": "failed",
                "reason": "missing_pane_id",
                "operation_id": operation.operation_id,
                "terminal_reason": terminal_reason,
                "reap_error": error,
            }

        live_peer = await self._registry.get_peer(str(peer_id)) if peer_id else None
        if live_peer is not None and live_peer.pane_id and live_peer.pane_id != pane_id:
            error = {
                "reason": "release_handle_mismatch",
                "peer_id": peer_id,
                "expected_pane_id": pane_id,
                "actual_pane_id": live_peer.pane_id,
            }
            self._operations.fail(operation.operation_id, strategy="kill_pane", error=error)
            return {
                "status": "failed",
                "reason": "release_handle_mismatch",
                "operation_id": operation.operation_id,
                "terminal_reason": terminal_reason,
                "reap_error": error,
            }

        pane_was_live = probe_tmux_pane(pane_id) is not None
        killed = kill_pane(pane_id)
        if killed:
            forget_spawn_ownership(pane_id)
            if peer_id:
                await self._registry.unregister_peer(str(peer_id))
            result = {
                "status": "released",
                "reason": "pane_killed",
                "operation_id": operation.operation_id,
                "terminal_reason": terminal_reason,
                "peer_id": peer_id,
                "pane_id": pane_id,
                "released_at": now_iso(),
            }
            self._operations.complete(
                operation.operation_id,
                strategy="kill_pane",
                result=result,
            )
            return result

        if pane_was_live:
            error = {
                "reason": "kill_failed",
                "peer_id": peer_id,
                "pane_id": pane_id,
            }
            self._operations.fail(operation.operation_id, strategy="kill_pane", error=error)
            return {
                "status": "failed",
                "reason": "kill_failed",
                "operation_id": operation.operation_id,
                "terminal_reason": terminal_reason,
                "peer_id": peer_id,
                "pane_id": pane_id,
                "reap_error": error,
            }

        result = {
            "status": "already_released",
            "reason": "pane_not_live",
            "operation_id": operation.operation_id,
            "terminal_reason": terminal_reason,
            "peer_id": peer_id,
            "pane_id": pane_id,
            "released_at": now_iso(),
        }
        forget_spawn_ownership(pane_id)
        if live_peer is not None and peer_id:
            await self._registry.unregister_peer(str(peer_id))
        self._operations.complete(
            operation.operation_id,
            strategy="kill_pane",
            result=result,
        )
        return result

    async def _find_live_peer(
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
                if _utcnow() >= deadline:
                    return None
                await asyncio.sleep(0.25)
                continue
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
        # Shared resume-safety seam (same as restart + job_runner): only attach a
        # plan when the id is pre-validated on disk, else fall back to fresh.
        decision = resolve_resume_safety(
            backend=backend,
            path=path,
            runtime_session_id=runtime_session_id,
            repowire_session_id=(
                repowire_session_id if isinstance(repowire_session_id, str) else None
            ),
            capability=capability if isinstance(capability, dict) else None,
        )
        return decision.plan

    @staticmethod
    def resume_plan_info(plan: AgentResumePlan | None) -> dict[str, Any] | None:
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
        operation_id: str | None = None,
        process_scope: str | None = None,
        continuity: str | None = None,
    ) -> dict[str, Any]:
        binding = self.runtime_binding_for_peer(peer, work=work)
        binding["source"] = source
        binding["recorded_at"] = now_iso()
        if work.source_id:
            binding["source_id"] = work.source_id
        if work.source_kind:
            binding["source_kind"] = work.source_kind
        if operation_id:
            binding["acquired_by_operation_id"] = operation_id
        if process_scope:
            binding["process_scope"] = process_scope
        if continuity:
            binding["continuity"] = continuity
        if self._calendar is not None and work.source_kind == "calendar" and work.source_id:
            self._calendar.update_runtime_binding(work.source_id, binding=binding)
        return binding

    def runtime_binding_for_peer(
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
                    backend=peer.backend.value,
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
    def _execution_policy(work: TrackedWork) -> dict[str, str]:
        execution = (work.request or {}).get("execution") or {}
        process_scope = execution.get("process_scope")
        target = execution.get("target") or {}
        if (
            process_scope is None
            and not (target.get("assigned_peer_id") or work.assigned_peer_id)
            and target.get("path")
            and target.get("backend")
        ):
            process_scope = "per_fire"
        process_scope = str(process_scope or "persistent")
        if process_scope == "per-fire":
            process_scope = "per_fire"
        continuity = execution.get("continuity")
        if continuity is None and process_scope == "per_fire":
            continuity = "resume" if work.source_kind == "calendar" else "fresh"
        continuity = str(continuity or "resume")
        return {"process_scope": process_scope, "continuity": continuity}

    @staticmethod
    def _release_handle_for_peer(
        peer: Peer,
        *,
        process_scope: str,
        operation_id: str,
        strategy: str,
    ) -> dict[str, Any] | None:
        if process_scope != "per_fire":
            return None
        if not peer.pane_id:
            return None
        return {
            "kind": "tmux_pane",
            "peer_id": peer.peer_id,
            "display_name": peer.display_name,
            "pane_id": peer.pane_id,
            "tmux_session": peer.tmux_session,
            "operation_id": operation_id,
            "strategy": strategy,
            "created_at": now_iso(),
        }

    @staticmethod
    def _normalize_path(path: str | None) -> str:
        if not path:
            return ""
        return str(Path(path).expanduser().resolve())
