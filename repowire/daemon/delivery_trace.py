"""Delivery trace ledger: a durable, locally-inspectable record of how a
message (ask/notify) moved through delivery stages.

One row per stage. Rows for a single message share a ``trace_id``
(correlation_id for asks, delivery_id for notifies) and carry a monotonic
``seq`` so stages render in true order even when ISO timestamps tie.

Writes are best-effort: tracing must never break delivery, so ``record``
swallows and logs storage errors rather than propagating them.

Stored in a dedicated SQLite table (not the dashboard event log) because
trace volume would otherwise evict the 500-item event buffer and spam the
realtime stream.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Literal
from uuid import uuid4

if TYPE_CHECKING:
    from repowire.daemon.state.database import StateDatabase

logger = logging.getLogger(__name__)

TraceStage = Literal[
    "created",
    "resolved_peer",
    "routed",
    "websocket_sent",
    "hook_received",
    "pane_injected",
    "pending",
    "acked",
    "closed",
    # failure stages:
    "resolve_failed",
    "no_connection",
    "injection_failed",
]

TraceKind = Literal["ask", "notify"]
TraceStatus = Literal["ok", "fail"]


@dataclass
class TraceRow:
    """One recorded delivery stage."""

    trace_id: str
    delivery_id: str | None
    seq: int
    kind: str
    stage: str
    status: str
    peer_id: str | None
    from_peer_id: str | None
    ts: str
    detail: dict


class DeliveryTraceStore:
    """SQLite-backed delivery trace ledger."""

    def __init__(self, db: StateDatabase) -> None:
        self._conn = db.conn

    def record(
        self,
        *,
        trace_id: str,
        kind: TraceKind,
        stage: TraceStage,
        status: TraceStatus = "ok",
        delivery_id: str | None = None,
        peer_id: str | None = None,
        from_peer_id: str | None = None,
        detail: dict | None = None,
    ) -> None:
        """Append one stage row. Best-effort: never raises on storage failure."""
        try:
            ts = datetime.now(timezone.utc).isoformat()
            detail_json = json.dumps(detail or {}, separators=(",", ":"), sort_keys=True)
            # Compute the per-trace seq inside the INSERT as a scalar subquery so
            # the read and write are one atomic statement (no select-then-insert
            # race for the next ordinal).
            with self._conn:
                self._conn.execute(
                    """
                    INSERT INTO delivery_traces(
                        id, trace_id, delivery_id, seq, kind, stage, status,
                        peer_id, from_peer_id, ts, detail_json
                    ) VALUES (
                        ?, ?, ?,
                        (SELECT COALESCE(MAX(seq), -1) + 1
                         FROM delivery_traces WHERE trace_id = ?),
                        ?, ?, ?, ?, ?, ?, ?
                    )
                    """,
                    (
                        uuid4().hex,
                        trace_id,
                        delivery_id or trace_id,
                        trace_id,
                        kind,
                        stage,
                        status,
                        peer_id,
                        from_peer_id,
                        ts,
                        detail_json,
                    ),
                )
        except Exception:  # noqa: BLE001 - tracing must not break delivery
            logger.warning("Failed to record delivery trace stage %s", stage, exc_info=True)

    def stages_for(self, trace_id: str) -> list[TraceRow]:
        """All recorded stages for a trace, in seq order (ties resolved by seq)."""
        rows = self._conn.execute(
            """
            SELECT trace_id, delivery_id, seq, kind, stage, status,
                   peer_id, from_peer_id, ts, detail_json
            FROM delivery_traces
            WHERE trace_id = ?
            ORDER BY seq ASC
            """,
            (trace_id,),
        ).fetchall()
        return [self._row_to_trace(r) for r in rows]

    def latest_stage(self, *, peer_id: str, stage: TraceStage) -> TraceRow | None:
        """Most recent row for a peer at a given stage (for inbound-health derivation)."""
        row = self._conn.execute(
            """
            SELECT trace_id, delivery_id, seq, kind, stage, status,
                   peer_id, from_peer_id, ts, detail_json
            FROM delivery_traces
            WHERE peer_id = ? AND stage = ?
            ORDER BY ts DESC
            LIMIT 1
            """,
            (peer_id, stage),
        ).fetchone()
        return self._row_to_trace(row) if row is not None else None

    def prune(self, before_ts: str) -> int:
        """Delete rows older than ``before_ts`` (ISO8601). Returns rows removed."""
        with self._conn:
            cur = self._conn.execute(
                "DELETE FROM delivery_traces WHERE ts < ?",
                (before_ts,),
            )
            return cur.rowcount or 0

    def record_outcome(
        self,
        *,
        trace_id: str,
        kind: TraceKind,
        peer_id: str | None,
        transport: str,
        hook_delivery: dict | None,
        delivery_id: str | None = None,
        from_peer_id: str | None = None,
    ) -> None:
        """Record the terminal delivery stage(s) from a transport outcome.

        Data-driven so callers don't hand-assert injection. Truth table:
          - ws + hook_delivery.status == injected -> hook_received, pane_injected
          - ws + hook_delivery.status in {failed, rejected} -> hook_received, injection_failed
          - ws + no hook_delivery (legacy hook, no ack) -> websocket_sent (unverified)
          - acp (no ws hook) -> websocket_sent (delivered, unverified at pane)
        """
        def _rec(stage: TraceStage, status: TraceStatus = "ok", detail: dict | None = None) -> None:
            self.record(
                trace_id=trace_id, kind=kind, stage=stage, status=status,
                delivery_id=delivery_id, peer_id=peer_id, from_peer_id=from_peer_id,
                detail=detail,
            )

        hook_status = hook_delivery.get("status") if isinstance(hook_delivery, dict) else None
        if hook_status == "injected":
            _rec("hook_received")
            _rec("pane_injected")
        elif hook_status in ("failed", "rejected"):
            _rec("hook_received")
            _rec("injection_failed", status="fail", detail={"hook_status": hook_status})
        else:
            # No terminal hook receipt: ACP, or a legacy hook that never acks.
            # Record handoff only; do NOT claim pane injection.
            _rec("websocket_sent", detail={"transport": transport, "verified": False})

    @staticmethod
    def _row_to_trace(row) -> TraceRow:
        try:
            detail = json.loads(row["detail_json"])
            if not isinstance(detail, dict):
                detail = {}
        except (json.JSONDecodeError, TypeError):
            detail = {}
        return TraceRow(
            trace_id=row["trace_id"],
            delivery_id=row["delivery_id"],
            seq=int(row["seq"]),
            kind=row["kind"],
            stage=row["stage"],
            status=row["status"],
            peer_id=row["peer_id"],
            from_peer_id=row["from_peer_id"],
            ts=row["ts"],
            detail=detail,
        )
