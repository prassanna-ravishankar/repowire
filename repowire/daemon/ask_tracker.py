"""Ask/ack lifecycle tracking for the Repowire daemon.

The AskTracker is the source of truth for open ask threads. Asks are
registered when a peer fires `ask()`, the recipient transport injects the
wire frame on receipt, and the ask is closed when the recipient either:

  - calls `ack(corr_id)` — bare close, no content
  - calls `ack(corr_id, msg)` — close with reply content delivered to asker
  - calls `ask(reply_to=corr_id, ...)` — opens a new thread + closes this one

Open asks targeting a peer are surfaced on every Stop hook poll via
`/asks/pending`, so an agent that hasn't acked will be reminded on every
subsequent turn until they do. Reply delivery is handled by the
notification pipeline (framed `[ack #cid from @peer] msg`).

In-memory only. Asks are evicted after 24h via TTL sweep mirroring the
config's prune_max_age_hours.
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from uuid import uuid4

from repowire.protocol.questions import Answer, Question

logger = logging.getLogger(__name__)

_EVICTION_INTERVAL_SECONDS = 300.0


class QuiesceFailedError(Exception):
    """Raised when begin_quiesce cannot acquire the barrier because open
    asks already exist for the peer."""

    def __init__(self, peer_id: str, open_cids: list[str]) -> None:
        super().__init__(f"Peer {peer_id} has open asks: {open_cids}")
        self.peer_id = peer_id
        self.open_cids = open_cids


class QuiescedError(Exception):
    """Raised when register() is called while a peer is mid-switch."""

    def __init__(self, peer_id: str) -> None:
        super().__init__(f"Peer {peer_id} is mid-switch; ask refused")
        self.peer_id = peer_id


@dataclass(frozen=True)
class AskerIdentity:
    """Snapshot of the asker's stable identity at stash time.

    Captured only when the asker peer has *all* of these fields present and
    non-default (notably ``machine != "unknown"`` and a non-empty normalized
    path). Used by the registry's identity-tuple rebind pass to redeliver
    stashed replies after the original peer_id has been pruned/reaped.
    """

    display_name: str
    circle: str
    backend: str  # AgentType.value
    path: str  # normalized via os.path.normpath(os.path.realpath(...))
    machine: str


@dataclass
class Ask:
    """An open ask awaiting ack/reply.

    close_reason: 'ack' | 'ack_with_msg' | 'reply_to' | 'evicted' | 'send_failed'.

    ``pending_reply`` is the durable home for a completed reply whose first
    delivery attempt failed because the asker had no live transport. The
    ACP-routed `/ask` path uses this to retain a successful ACP answer across
    asker reconnects — the WS path doesn't need it because /ack's MCP caller
    handles retry on 503. Cleared on successful redelivery (which also closes
    the ask).

    ``asker_identity`` is populated alongside ``pending_reply`` when the asker
    had a complete stable identity tuple at stash time. The registry uses it
    to rebind redelivery to a new peer_id after a clean-takeover prune. When
    None, only same-peer_id reconnect can redeliver.
    """

    correlation_id: str
    from_peer_id: str
    from_peer_name: str
    to_peer_id: str
    to_peer_name: str
    text: str
    from_repowire_session_id: str | None = None
    to_repowire_session_id: str | None = None
    reply_to: str | None = None
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    closed: bool = False
    close_reason: str | None = None
    pending_reply: str | None = None
    asker_identity: AskerIdentity | None = None
    pending_reply_at: datetime | None = None
    # ``reply_text`` is the captured ack message body (the normal ack path
    # frames+notifies+closes but does not retain the body); populated only
    # after the reply is resolved, so a 503'd push reply leaves the ask
    # showing pending. ``parent_id`` marks ask-many fanout children.
    parent_id: str | None = None
    reply_text: str | None = None
    replied_at: datetime | None = None
    # Reply routing: "push" (default) frames + notifies the asker's transport;
    # "pull" retains the reply on the ask and skips the notify — set when the
    # asker is blocked in wait_on_ack, so the reply arrives as the tool result
    # instead of racing a pane injection.
    reply_delivery: str = "push"
    # Structured question/answer (mesh questions primitive). ``question`` is set
    # when the ask is a structured question (approval / choice / text); ``answer``
    # is the typed resolution. A plain ask leaves both None and behaves as today.
    question: Question | None = None
    answer: Answer | None = None


class AskTracker:
    """In-memory store of open ask threads.

    All mutating methods are async-locked. Read methods are unlocked snapshots
    (callers that need consistency should re-check after acting).
    """

    def __init__(self, *, ttl_hours: float = 24.0) -> None:
        self._lock = asyncio.Lock()
        self._asks: dict[str, Ask] = {}
        self._ttl = timedelta(hours=ttl_hours)
        self._last_eviction: float = 0.0
        # Live answer waiters for blocking-transport questions. The future is a
        # convenience for transports that own a suspendable call (ACP, PreToolUse
        # hook); ``ask.answer`` is the source of truth. Futures vanish on restart
        # like any open ask.
        self._answer_waiters: dict[str, asyncio.Future[Answer]] = {}
        # peer_ids currently mid-switch — new asks to/from them are refused so
        # the switch route can kill+respawn without orphaning correlation IDs.
        self._quiescing: set[str] = set()


    async def begin_quiesce(self, peer_id: str) -> None:
        """Atomically: verify no open asks for peer, mark peer as quiescing.

        While quiesced, register() rejects new asks for this peer in either
        direction. Use end_quiesce() in a finally to release.

        Exclusive: a concurrent begin_quiesce for the same peer_id raises
        QuiesceFailedError with an empty open_cids list, so only one caller
        ever holds the barrier at a time. Without this, two concurrent
        switches against the same peer would both enter the critical
        kill/spawn section and the first end_quiesce would prematurely
        release the barrier for the second.

        Raises QuiesceFailedError if open asks exist OR the peer is already
        quiescing.
        """
        async with self._lock:
            if peer_id in self._quiescing:
                raise QuiesceFailedError(peer_id, [])
            open_asks = [
                a.correlation_id for a in self._asks.values()
                if not a.closed and (a.to_peer_id == peer_id or a.from_peer_id == peer_id)
            ]
            if open_asks:
                raise QuiesceFailedError(peer_id, open_asks)
            self._quiescing.add(peer_id)
            logger.debug("Quiesced peer %s for switch", peer_id)

    async def end_quiesce(self, peer_id: str) -> None:
        """Release the switch barrier. Idempotent."""
        async with self._lock:
            self._quiescing.discard(peer_id)
            logger.debug("Released quiesce for peer %s", peer_id)

    async def register(
        self,
        from_peer_id: str,
        from_peer_name: str,
        to_peer_id: str,
        to_peer_name: str,
        text: str,
        reply_to: str | None = None,
        correlation_id: str | None = None,
        from_repowire_session_id: str | None = None,
        to_repowire_session_id: str | None = None,
        parent_id: str | None = None,
        question: Question | None = None,
        reply_delivery: str = "push",
    ) -> str:
        """Register a new open ask. Returns the correlation_id.

        If correlation_id is supplied and already exists, this is treated as
        a retry: the existing entry is preserved (lifecycle state intact)
        and the same id is returned. Auto-generated cids never collide.

        ``parent_id`` links this ask as a child of an ask-many parent.
        ``question`` makes this ask a structured question (approval / choice /
        text); a blocking transport then awaits :meth:`wait_for_answer`.
        ``reply_delivery="pull"`` retains the eventual reply on the ask instead
        of notifying the asker's transport (job dispatch, wait_on_ack).

        Raises QuiescedError if either endpoint is mid-switch.
        """
        async with self._lock:
            if to_peer_id in self._quiescing:
                raise QuiescedError(to_peer_id)
            if from_peer_id in self._quiescing:
                raise QuiescedError(from_peer_id)
            cid = correlation_id or f"ask-{uuid4().hex[:8]}"
            if cid in self._asks:
                logger.debug("Ask %s already registered; treating as retry", cid)
                return cid
            self._asks[cid] = Ask(
                correlation_id=cid,
                from_peer_id=from_peer_id,
                from_peer_name=from_peer_name,
                to_peer_id=to_peer_id,
                to_peer_name=to_peer_name,
                text=text,
                from_repowire_session_id=from_repowire_session_id,
                to_repowire_session_id=to_repowire_session_id,
                reply_to=reply_to,
                parent_id=parent_id,
                question=question,
                reply_delivery=reply_delivery,
            )
            logger.debug("Registered ask %s: %s -> %s", cid, from_peer_name, to_peer_name)
            return cid

    async def capture_reply(self, correlation_id: str, reply_text: str) -> None:
        """Record an ack message body on an ask once the reply is resolved.

        The push ack path frames + notifies + closes but does not retain the
        body; ask-many aggregation and wait_on_ack pulls need it. No-op when
        already captured. For push delivery, call only after the framed reply
        was delivered.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None or ask.reply_text is not None:
                return
            ask.reply_text = reply_text
            ask.replied_at = datetime.now(timezone.utc)

    async def list_by_parent(self, parent_id: str) -> list[Ask]:
        """Return all child asks for an ask-many parent (snapshot)."""
        return [a for a in self._asks.values() if a.parent_id == parent_id]

    class AlreadyAnsweredError(Exception):
        """Raised by :meth:`answer` when the question was already answered."""

    def _cancel_waiter(self, correlation_id: str, *, message: str | None = None) -> None:
        """Resolve a dangling answer waiter with a cancelled Answer. Hold lock.

        Called when an ask reaches a terminal state (evict/forget/close/rebind)
        while a blocking transport is still awaiting it, so the waiter doesn't
        hang until its own timeout (or forever, if it had none). The waiter's
        caller sees outcome="cancelled" (with the close reason as the message).
        """
        waiter = self._answer_waiters.pop(correlation_id, None)
        if waiter is not None and not waiter.done():
            waiter.set_result(Answer(outcome="cancelled", message=message))

    async def answer(self, correlation_id: str, answer: Answer) -> Ask:
        """Record the typed answer to a question ask. First valid answer wins.

        Stores ``ask.answer`` (the source of truth), closes the ask, and resolves
        any live waiter future. Validates the answer against the question's
        options. Raises ``KeyError`` for an unknown id, ``ValueError`` for an
        invalid answer, and ``AlreadyAnsweredError`` if already resolved.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None:
                raise KeyError(correlation_id)
            # Guard on closed, not just answer-present: an ask can be closed by
            # another terminal path (send_failed / ack / reply_to) with no answer
            # recorded. Answering it would overwrite that close state (gemini
            # review). First terminal state wins.
            if ask.closed or ask.answer is not None:
                raise self.AlreadyAnsweredError(correlation_id)
            if ask.question is not None:
                error = ask.question.validate_answer(answer)
                if error is not None:
                    raise ValueError(error)
            ask.answer = answer
            if answer.text is not None and ask.reply_text is None:
                ask.reply_text = answer.text
                ask.replied_at = datetime.now(timezone.utc)
            ask.closed = True
            ask.close_reason = "answered"
            waiter = self._answer_waiters.pop(correlation_id, None)
        if waiter is not None and not waiter.done():
            waiter.set_result(answer)
        return ask

    async def wait_for_answer(
        self,
        correlation_id: str,
        *,
        timeout_seconds: float | None,
        default_answer: Answer | None,
    ) -> Answer:
        """Block until ``correlation_id`` is answered, or apply the default on timeout.

        Only meaningful for a transport that owns a suspendable call (ACP, a
        PreToolUse hook). The returned answer is also recorded on the ask via the
        timeout path so the ledger stays truthful. If the ask was already
        answered (race), returns that answer immediately.
        """
        resolved_default = default_answer or Answer(outcome="timed_out")
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None:
                raise KeyError(correlation_id)
            if ask.answer is not None:
                return ask.answer
            if ask.closed:
                return Answer(outcome="cancelled", message=ask.close_reason)
            # Fail loud NOW if the default is invalid for this question, rather
            # than silently returning an unrecorded answer at timeout (codex
            # review): the timeout path must always go through answer().
            if ask.question is not None:
                error = ask.question.validate_answer(resolved_default)
                if error is not None:
                    raise ValueError(f"invalid default_answer: {error}")
            waiter = self._answer_waiters.get(correlation_id)
            if waiter is None:
                waiter = asyncio.get_running_loop().create_future()
                self._answer_waiters[correlation_id] = waiter
        try:
            return await asyncio.wait_for(asyncio.shield(waiter), timeout=timeout_seconds)
        except asyncio.TimeoutError:
            try:
                await self.answer(correlation_id, resolved_default)
            except (self.AlreadyAnsweredError, KeyError):
                # A real answer landed in the race window, or the ask was
                # evicted (which already cancelled this waiter).
                existing = await self.get(correlation_id)
                if existing is not None and existing.answer is not None:
                    return existing.answer
            return resolved_default

    async def wait_for_resolution(
        self,
        correlation_id: str,
        *,
        timeout_seconds: float,
        pull: bool = True,
    ) -> Ask | None:
        """Bounded wait until the ask resolves (answered or closed); None on timeout.

        Unlike :meth:`wait_for_answer`, a timeout records nothing — the ask
        stays open, so callers can poll in bounded slices (wait_on_ack). When
        ``pull`` is set, the still-open ask is switched to pull reply delivery
        so a racing ack retains the reply on the ask instead of injecting it
        into the asker's pane while the asker is blocked here.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None:
                raise KeyError(correlation_id)
            if ask.closed:
                return ask
            if pull:
                ask.reply_delivery = "pull"
            waiter = self._answer_waiters.get(correlation_id)
            if waiter is None:
                waiter = asyncio.get_running_loop().create_future()
                self._answer_waiters[correlation_id] = waiter
        try:
            await asyncio.wait_for(asyncio.shield(waiter), timeout=timeout_seconds)
        except asyncio.TimeoutError:
            return None
        return self._asks.get(correlation_id)

    async def close(self, correlation_id: str, reason: str) -> Ask | None:
        """Close an ask. Returns the Ask if it existed and wasn't already closed."""
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask or ask.closed:
                return None
            ask.closed = True
            ask.close_reason = reason
            # A structured question closed through a non-answer terminal path
            # (ack/send_failed/reply_to) must not leave a blocking transport
            # waiting forever (codex review). Resolve any waiter as cancelled.
            self._cancel_waiter(correlation_id, message=reason)
            logger.debug("Closed ask %s: %s", correlation_id, reason)
            return ask

    async def get(self, correlation_id: str) -> Ask | None:
        """Look up an ask by corr_id."""
        return self._asks.get(correlation_id)

    async def set_pending_reply(
        self,
        correlation_id: str,
        framed_reply: str,
        identity: AskerIdentity | None = None,
        *,
        allow_answered_question: bool = False,
    ) -> bool:
        """Stash a completed reply for later redelivery.

        Used by the ACP-routed `/ask` path when notify fails with
        TransportError: the assembled ACP answer is real and must not be
        dropped just because the asker is currently offline. Returns True
        if the ask exists and was still open, False otherwise.

        ``allow_answered_question`` is the structured-question escape hatch:
        `/answer` records the typed answer first (the source of truth), closing
        the ask with ``close_reason="answered"`` before it attempts the
        human-readable notify back to the asker. If that notify hits an offline
        transport, this flag lets the already-answered question carry a
        pending redelivery without reopening it. It deliberately does *not*
        permit stashing on arbitrary closed asks.

        ``identity`` is the asker's full stable identity tuple at stash time,
        captured by the route handler when the asker peer is resolvable and
        has all required fields. When provided, it enables the registry's
        pass-2 identity-tuple rebind on asker re-registration.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if not ask:
                return False
            if ask.closed and not (
                allow_answered_question
                and ask.question is not None
                and ask.answer is not None
                and ask.close_reason == "answered"
            ):
                return False
            ask.pending_reply = framed_reply
            ask.asker_identity = identity
            ask.pending_reply_at = datetime.now(timezone.utc)
            logger.debug(
                "Stashed pending reply on ask %s (identity=%s)",
                correlation_id, "yes" if identity else "no",
            )
            return True

    async def mark_pending_reply_delivered(
        self,
        correlation_id: str,
        *,
        new_from_peer_id: str | None = None,
        reason: str = "ack_with_msg",
    ) -> bool:
        """Mark a stashed reply delivered and clear it.

        Open asks are closed with ``reason``. Already answered structured
        questions stay closed as ``answered``; only their transient stash is
        cleared. ``new_from_peer_id`` is used by the registry's identity-rebind
        pass after a clean-takeover gives the original asker a new peer id.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None or ask.pending_reply is None:
                return False
            if new_from_peer_id is not None:
                ask.from_peer_id = new_from_peer_id
            if not ask.closed:
                ask.closed = True
                ask.close_reason = reason
                self._cancel_waiter(correlation_id, message=reason)
            ask.pending_reply = None
            logger.debug(
                "Marked pending reply delivered for ask %s%s",
                correlation_id,
                f" -> {new_from_peer_id}" if new_from_peer_id else "",
            )
            return True

    async def clear_pending_reply(self, correlation_id: str) -> None:
        """Drop the stashed reply from an ask (idempotent).

        Used after successful redelivery so the closed Ask object doesn't
        retain reply text it can never use again.
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is not None:
                ask.pending_reply = None

    async def rebind_and_close(
        self,
        correlation_id: str,
        new_from_peer_id: str,
        reason: str,
    ) -> bool:
        """Atomic rebind + close + clear under the tracker lock.

        Called by the registry's pass-2 redelivery only after notify
        succeeds. Rewrites ``from_peer_id`` to the rebound asker,
        closes the ask, and clears the stash in a single critical
        section so readers never observe a half-rebound state.

        Returns True if the ask was open and the mutation applied,
        False if the ask is missing or already closed (in which case
        no mutation happened).
        """
        async with self._lock:
            ask = self._asks.get(correlation_id)
            if ask is None or ask.closed:
                return False
            ask.from_peer_id = new_from_peer_id
            ask.closed = True
            ask.close_reason = reason
            ask.pending_reply = None
            self._cancel_waiter(correlation_id, message=reason)
            logger.debug(
                "Rebound + closed ask %s -> %s (%s)",
                correlation_id, new_from_peer_id, reason,
            )
            return True

    async def take_pending_replies_for_asker(self, peer_id: str) -> list[Ask]:
        """Snapshot asks targeting this asker that have a stashed reply.

        Caller (peer_registry reconnect path) attempts redelivery for each;
        on success it marks the reply delivered and the reply will not be
        re-driven. We snapshot rather than drain so a failed redelivery leaves
        the reply in place for the next reconnect.
        """
        async with self._lock:
            return [
                ask for ask in self._asks.values()
                if ask.from_peer_id == peer_id
                and ask.pending_reply is not None
            ]

    async def take_orphan_pending_replies_matching(
        self,
        *,
        display_name: str,
        circle: str,
        backend: str,
        path: str,
        machine: str,
        live_peer_ids: set[str],
    ) -> list[Ask]:
        """Pure filter for identity-tuple rebind.

        Returns asks where:
          * ``asker_identity`` is non-None
          * every identity field matches the supplied tuple exactly
          * ``from_peer_id not in live_peer_ids`` (the original asker peer_id
            is no longer registered — only then is rebind in play)
          * the ask has a stashed reply

        The tracker performs no liveness lookups and no uniqueness gating.
        The caller (PeerRegistry) supplies ``live_peer_ids`` from its own
        snapshot and applies any cross-peer ambiguity gate before
        delivering.
        """
        async with self._lock:
            out: list[Ask] = []
            for ask in self._asks.values():
                if ask.pending_reply is None:
                    continue
                ident = ask.asker_identity
                if ident is None:
                    continue
                if (
                    ident.display_name != display_name
                    or ident.circle != circle
                    or ident.backend != backend
                    or ident.path != path
                    or ident.machine != machine
                ):
                    continue
                if ask.from_peer_id in live_peer_ids:
                    continue
                out.append(ask)
            return out

    async def snapshot_pending_replies_for_peer(self, peer_id: str) -> list[Ask]:
        """Pure read: asks involving this peer that carry a stashed reply.

        Used by ``PeerRegistry._reap_dangling_peers`` / ``_evict_stale_peers``
        before they call ``forget_peer``, so the registry can emit one
        ``pending_reply_lost`` event per doomed stash. Does not mutate.
        """
        async with self._lock:
            return [
                ask for ask in self._asks.values()
                if ask.pending_reply is not None
                and (ask.to_peer_id == peer_id or ask.from_peer_id == peer_id)
            ]

    async def snapshot_expired_pending_replies(self) -> list[Ask]:
        """Pure read: TTL-expired asks that still carry a stashed reply.

        Used by ``PeerRegistry.lazy_repair`` to emit ``pending_reply_lost``
        events before ``evict_expired`` deletes the entries. Single owner of
        TTL-loss emission. Does not mutate. Recipient-facing
        ``pending_for_peer``'s lazy eviction skips stashed-expired asks so
        the registry-driven path always wins.
        """
        cutoff = datetime.now(timezone.utc) - self._ttl
        async with self._lock:
            return [
                ask for ask in self._asks.values()
                if ask.created_at < cutoff and ask.pending_reply is not None
            ]

    async def pending_for_peer(
        self,
        peer_id: str,
        max_results: int = 10,
        direction: str = "inbound",
    ) -> list[Ask]:
        """Return open asks for this peer, newest first, capped at max_results.

        direction:
          - "inbound"  (default): asks targeting this peer (to_peer_id == peer_id)
          - "outbound": asks this peer opened (from_peer_id == peer_id)
          - "both":    union of the two

        Lazy-repair: opportunistically evicts TTL-expired asks at most once
        per _EVICTION_INTERVAL_SECONDS. Stop hooks call this on each response,
        so the dict gets swept regularly without a background timer.
        """
        if direction not in ("inbound", "outbound", "both"):
            raise ValueError(f"invalid direction: {direction!r}")
        await self._maybe_evict_expired()
        async with self._lock:
            def matches(ask: Ask) -> bool:
                if direction == "inbound":
                    return ask.to_peer_id == peer_id
                if direction == "outbound":
                    return ask.from_peer_id == peer_id
                return ask.to_peer_id == peer_id or ask.from_peer_id == peer_id

            candidates = [
                ask for ask in self._asks.values()
                if matches(ask) and not ask.closed
            ]
            candidates.sort(key=lambda a: a.created_at, reverse=True)
            return candidates[:max_results]

    async def _maybe_evict_expired(self) -> None:
        """Run TTL eviction if enough wall time has passed since the last sweep.

        Skips asks that still carry a stashed pending reply: those are
        owned exclusively by the registry's ``lazy_repair`` sweep, which
        snapshots → emits ``pending_reply_lost`` → calls
        ``evict_expired(include_stashed=True)`` in order. If this
        Stop-hook-triggered path dropped stashed-expired asks first, the
        loss would be silent.
        """
        now = time.monotonic()
        if now - self._last_eviction < _EVICTION_INTERVAL_SECONDS:
            return
        self._last_eviction = now
        await self.evict_expired(include_stashed=False)

    async def forget_peer(self, peer_id: str) -> int:
        """Drop any asks involving this peer.

        Called by PeerRegistry when pruning offline peers, so the tracker's
        memory footprint is bounded by the live peer set.

        Returns the number of asks dropped.
        """
        async with self._lock:
            doomed = [
                cid for cid, ask in self._asks.items()
                if ask.to_peer_id == peer_id or ask.from_peer_id == peer_id
            ]
            for cid in doomed:
                ask = self._asks[cid]
                if not ask.closed:
                    ask.closed = True
                    ask.close_reason = "evicted"
                self._cancel_waiter(cid)
                del self._asks[cid]
            return len(doomed)

    async def evict_expired(self, *, include_stashed: bool = True) -> int:
        """Drop asks older than TTL. Returns count evicted.

        Closes them as 'evicted' before removal so any caller holding a
        reference can see why they vanished.

        ``include_stashed`` controls whether TTL-expired asks that still
        carry a stashed reply are dropped. Defaults to True for the
        registry-driven sweep (which has already emitted
        ``pending_reply_lost`` from the snapshot it took just before this
        call). The recipient-facing lazy path (``_maybe_evict_expired``)
        passes False so stashed-expired asks survive until the registry's
        single-owner emission path runs.
        """
        cutoff = datetime.now(timezone.utc) - self._ttl
        async with self._lock:
            expired = [
                cid for cid, ask in self._asks.items()
                if ask.created_at < cutoff
                and (include_stashed or ask.pending_reply is None)
            ]
            for cid in expired:
                ask = self._asks[cid]
                if not ask.closed:
                    ask.closed = True
                    ask.close_reason = "evicted"
                self._cancel_waiter(cid)
                del self._asks[cid]
            if expired:
                logger.info("Evicted %d expired asks", len(expired))
            return len(expired)

    def open_count(self) -> int:
        """Number of open (non-closed) asks. For diagnostics."""
        return sum(1 for ask in self._asks.values() if not ask.closed)

    def total_count(self) -> int:
        """Total asks tracked (open + closed-but-retained). For diagnostics."""
        return len(self._asks)
