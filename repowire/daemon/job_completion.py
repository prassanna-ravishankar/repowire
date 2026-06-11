"""Structural fire completion for durable jobs.

The daemon — not the executor — decides when a job fire is done. The Stop
hook already posts the executor's turn pair (user prompt + assistant reply)
as chat turns; this service correlates them with the in-flight attempt:

- The dispatch prompt embeds ``job_id``/``attempt_id`` markers verbatim. A
  user-role chat turn carrying the marker *arms* the attempt (state
  ``running`` / phase ``turn_started``).
- The next assistant-role chat turn from the assigned peer completes the
  fire: the turn's final message becomes the recorded result.
- An executor that dies (terminal peer offline) with an active attempt fails
  the fire with ``executor_died``.

Nothing here is voluntary for the executor: ``job_update`` remains optional
enrichment, and explicitly moving the attempt off phase ``turn_started`` is
the escape hatch that holds a fire open across turn ends (externally-blocked
jobs). Waiting on peers should happen *inside* the turn via ``wait_on_ack``.

The service also owns job observability: every work state transition emits a
``job_state_changed`` event (store callback), and terminal states best-effort
notify the job's owner — owner peer, then creator, then the circle's
orchestrator. The durable work row stays the source of truth; notification
failure never affects it.
"""

from __future__ import annotations

import asyncio
import logging
import re
from typing import Any

from repowire.daemon.job_release import current_attempt_id, release_executor_if_needed
from repowire.daemon.work_store import TrackedWork, is_terminal_state
from repowire.protocol.peers import PeerRole, PeerStatus

logger = logging.getLogger(__name__)

# Ids are exactly 12 hex chars, so no trailing \b: pane injection can collapse
# the prompt's newlines, leaving the id butted against the next word
# ("...034dattempt_id:"). The fixed length disambiguates, and arming verifies
# the parsed ids against the store before acting.
_WORK_MARKER = re.compile(r"\b(work-[0-9a-f]{12})")
_ATTEMPT_MARKER = re.compile(r"\b(attempt-[0-9a-f]{12})")

RESULT_SUMMARY_MAX_CHARS = 2000
RESULT_MESSAGE_MAX_CHARS = 65536
NOTIFY_SUMMARY_MAX_CHARS = 300

#: Attempt phase that lets a Stop complete the fire. Arming sets it; an
#: executor that explicitly changes phase (job_update) opts out of
#: Stop-completion and must terminal-report itself (or die trying).
ARMED_PHASE = "turn_started"

_ACTIVE_STATES = ("delivered", "running")


class JobCompletionService:
    """Daemon-side fire lifecycle: arm on prompt, complete on Stop."""

    def __init__(
        self,
        *,
        work_store: Any,
        ask_tracker: Any | None = None,
        session_control: Any | None = None,
        peer_registry: Any | None = None,
        peer_delivery: Any | None = None,
    ) -> None:
        self._store = work_store
        self._ask_tracker = ask_tracker
        self._session_control = session_control
        self._registry = peer_registry
        self._delivery = peer_delivery
        self._sender_peer_id: str | None = None
        self._finalize_tasks: set[asyncio.Task] = set()

    def set_sender_peer_id(self, peer_id: str) -> None:
        """Notify terminal results from the registered @jobs service peer."""
        self._sender_peer_id = peer_id

    # ------------------------------------------------------------------
    # Store state-change callback (sync) → events + terminal side effects
    # ------------------------------------------------------------------

    def on_work_state_change(self, work: TrackedWork, prior_state: str | None) -> None:
        """Emit a job_state_changed event; schedule terminal finalization.

        Called synchronously by the work store after a committed state
        transition. Must never raise into the store.
        """
        try:
            if self._registry is not None:
                self._registry.add_event(
                    "job_state_changed", self._event_payload(work, prior_state),
                )
        except Exception:
            logger.exception("job_state_changed event emission failed for %s", work.work_id)
        if not is_terminal_state(work.state):
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            logger.warning(
                "No running loop to finalize terminal job %s (%s); ledger is "
                "still correct, owner notify skipped",
                work.work_id, work.state,
            )
            return
        task = loop.create_task(self._finalize(work))
        self._finalize_tasks.add(task)
        task.add_done_callback(self._finalize_tasks.discard)

    def _event_payload(self, work: TrackedWork, prior_state: str | None) -> dict[str, Any]:
        summary = work.result_summary
        if summary and len(summary) > NOTIFY_SUMMARY_MAX_CHARS:
            summary = summary[:NOTIFY_SUMMARY_MAX_CHARS] + "…"
        return {
            "work_id": work.work_id,
            "job_id": work.work_id,
            "title": work.title,
            "state": work.state,
            "prior_state": prior_state,
            "state_reason": work.state_reason,
            "phase": work.phase,
            "attempt_id": current_attempt_id(work),
            "assigned_peer_id": work.assigned_peer_id,
            "owner_peer_id": work.owner_peer_id,
            "created_by_peer_id": work.created_by_peer_id,
            "circle": work.circle,
            "correlation_id": work.correlation_id,
            "result_summary": summary,
            "error": work.error or None,
        }

    async def _finalize(self, work: TrackedWork) -> None:
        """Terminal side effects common to every path: ask hygiene + notify."""
        await self._close_dispatch_ask(work)
        await self._notify_owner(work)

    async def _close_dispatch_ask(self, work: TrackedWork) -> None:
        if self._ask_tracker is None or not work.correlation_id:
            return
        try:
            await self._ask_tracker.close(work.correlation_id, reason="fire_completed")
        except Exception:
            logger.exception(
                "Failed to close dispatch ask %s for job %s",
                work.correlation_id, work.work_id,
            )

    async def _notify_owner(self, work: TrackedWork) -> None:
        """Best-effort one-liner to whoever should know; ledger stays truth."""
        if self._delivery is None:
            return
        summary = (work.result_summary or work.state_reason or "").strip()
        if len(summary) > NOTIFY_SUMMARY_MAX_CHARS:
            summary = summary[:NOTIFY_SUMMARY_MAX_CHARS] + "…"
        text = (
            f"[job {work.work_id}] {work.title or work.kind} → {work.state}"
            + (f": {summary}" if summary else "")
            + f" (details: job_result {work.work_id})"
        )
        sender = self._sender_peer_id or "jobs"
        for target in await self._notify_targets(work):
            try:
                result = await self._delivery.notify_result(
                    from_peer=sender,
                    to_peer=target,
                    text=text,
                    bypass_circle=True,
                )
            except Exception as e:
                logger.info(
                    "Job %s terminal notify to %s failed (%s); trying next target",
                    work.work_id, target, e,
                )
                continue
            if getattr(result, "delivered", False) or getattr(result, "queued", False):
                return
        logger.info(
            "Job %s reached %s with no notifiable owner; result is in the ledger",
            work.work_id, work.state,
        )

    async def _notify_targets(self, work: TrackedWork) -> list[str]:
        targets = [
            peer_id
            for peer_id in (work.owner_peer_id, work.created_by_peer_id)
            if peer_id and peer_id != self._sender_peer_id
        ]
        orchestrator = await self._find_orchestrator(work.circle)
        if orchestrator and orchestrator not in targets:
            targets.append(orchestrator)
        return targets

    async def _find_orchestrator(self, circle: str | None) -> str | None:
        if self._registry is None:
            return None
        try:
            peers = await self._registry.get_all_peers()
        except Exception:
            return None
        for peer in peers:
            if peer.role != PeerRole.ORCHESTRATOR or peer.status == PeerStatus.OFFLINE:
                continue
            if circle and peer.circle != circle:
                continue
            return peer.peer_id
        return None

    # ------------------------------------------------------------------
    # Chat-turn correlation (arming + completion)
    # ------------------------------------------------------------------

    async def on_chat_turn(self, *, peer_id: str | None, role: str, text: str) -> None:
        """Correlate an ingested chat turn with in-flight job attempts."""
        if not peer_id or not text:
            return
        if role == "user":
            await self._maybe_arm(peer_id=peer_id, text=text)
        elif role == "assistant":
            await self._maybe_complete(peer_id=peer_id, text=text)

    async def _maybe_arm(self, *, peer_id: str, text: str) -> None:
        work_match = _WORK_MARKER.search(text)
        attempt_match = _ATTEMPT_MARKER.search(text)
        if not work_match or not attempt_match:
            return
        work = self._store.get(work_match.group(1))
        if work is None or work.state != "delivered":
            return
        attempt_id = attempt_match.group(1)
        if current_attempt_id(work) != attempt_id:
            return
        if work.assigned_peer_id and work.assigned_peer_id != peer_id:
            logger.warning(
                "Job %s prompt marker observed from %s but attempt is assigned "
                "to %s; not arming",
                work.work_id, peer_id, work.assigned_peer_id,
            )
            return
        self._store.update_state(
            work.work_id,
            state="running",
            state_reason="turn_started",
            phase=ARMED_PHASE,
            attempt_id=attempt_id,
        )
        logger.info("Job %s armed: fire turn started on %s", work.work_id, peer_id)

    async def _maybe_complete(self, *, peer_id: str, text: str) -> None:
        for work in self._armed_work_for_peer(peer_id):
            attempt_id = current_attempt_id(work)
            summary = text[:RESULT_SUMMARY_MAX_CHARS]
            try:
                self._store.update_attempt(
                    work.work_id,
                    attempt_id=attempt_id,
                    status="completed",
                    phase="completed",
                )
                updated = self._store.update_state(
                    work.work_id,
                    state="completed",
                    state_reason="turn_complete",
                    phase="completed",
                    result_summary=summary,
                    result_data={"final_message": text[:RESULT_MESSAGE_MAX_CHARS]},
                    attempt_id=attempt_id,
                )
            except (RuntimeError, ValueError) as e:
                # Another path (cancel, executor death, operator PATCH) won the
                # terminal race; their state stands.
                logger.info("Job %s completion superseded: %s", work.work_id, e)
                continue
            logger.info("Job %s completed: fire turn ended on %s", work.work_id, peer_id)
            # Close the dispatch ask inline (not just via the deferred
            # _finalize task): the Stop hook fetches /asks/pending right after
            # posting this chat turn, and an still-open dispatch ask would
            # block the executor's Stop with a spurious ack reminder.
            await self._close_dispatch_ask(updated or work)
            if updated is not None:
                await self._release(updated, terminal_reason="completed", attempt_id=attempt_id)

    def _armed_work_for_peer(self, peer_id: str) -> list[TrackedWork]:
        try:
            running = self._store.list_all(state="running")
        except Exception:
            logger.exception("Failed to list running work for completion check")
            return []
        return [
            work
            for work in running
            if work.assigned_peer_id == peer_id and work.phase == ARMED_PHASE
        ]

    # ------------------------------------------------------------------
    # Executor death → fire failure
    # ------------------------------------------------------------------

    async def on_peer_terminal_offline(self, peer_id: str, *, reason: str) -> None:
        """Fail in-flight fires whose executor is conclusively gone."""
        for state in _ACTIVE_STATES:
            try:
                works = self._store.list_all(state=state)
            except Exception:
                logger.exception("Failed to list %s work for executor-death check", state)
                continue
            for work in works:
                if work.assigned_peer_id != peer_id:
                    continue
                attempt_id = current_attempt_id(work)
                error = {
                    "reason": "executor_died",
                    "detail": reason,
                    "peer_id": peer_id,
                }
                try:
                    self._store.update_attempt(
                        work.work_id,
                        attempt_id=attempt_id,
                        status="failed",
                        phase="executor_died",
                        error=error,
                    )
                    updated = self._store.update_state(
                        work.work_id,
                        state="failed",
                        state_reason="executor_died",
                        phase="executor_died",
                        error=error,
                        attempt_id=attempt_id,
                    )
                except (RuntimeError, ValueError) as e:
                    logger.info(
                        "Job %s executor-death fail superseded: %s", work.work_id, e,
                    )
                    continue
                logger.warning(
                    "Job %s failed: executor %s went terminal offline (%s)",
                    work.work_id, peer_id, reason,
                )
                if updated is not None:
                    await self._release(
                        updated, terminal_reason="executor_died", attempt_id=attempt_id,
                    )

    async def reconcile_inflight(self, *, grace_seconds: float = 30.0) -> None:
        """One-shot post-restart reconcile: fail fires whose executor is gone.

        A daemon restart loses the terminal-offline signal for executors that
        died while it was down. Delayed by a grace window so live executors'
        ws-hooks can reconnect after the bounce before absence is judged.
        """
        if grace_seconds > 0:
            await asyncio.sleep(grace_seconds)
        if self._registry is None:
            return
        dead: set[str] = set()
        for state in _ACTIVE_STATES:
            try:
                works = self._store.list_all(state=state)
            except Exception:
                logger.exception("Failed to list %s work for restart reconcile", state)
                continue
            for work in works:
                peer_id = work.assigned_peer_id
                if not peer_id or peer_id in dead:
                    continue
                try:
                    peer = await self._registry.get_peer(peer_id)
                except Exception:
                    peer = None
                if peer is None or peer.status == PeerStatus.OFFLINE:
                    dead.add(peer_id)
        for peer_id in dead:
            await self.on_peer_terminal_offline(
                peer_id, reason="daemon_restart_reconcile",
            )

    async def _release(
        self, work: TrackedWork, *, terminal_reason: str, attempt_id: str | None,
    ) -> None:
        try:
            await release_executor_if_needed(
                work,
                work_store=self._store,
                session_control=self._session_control,
                terminal_reason=terminal_reason,
                attempt_id=attempt_id,
            )
        except Exception:
            logger.exception("Executor release failed for job %s", work.work_id)
