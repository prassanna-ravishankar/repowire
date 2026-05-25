"""SQLite-backed queued delivery store for polling peers."""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Literal
from uuid import uuid4

from repowire.daemon.state.database import StateDatabase

DeliveryKind = Literal["ask", "notify"]


@dataclass(frozen=True)
class QueuedDelivery:
    delivery_id: str
    peer_id: str
    repowire_session_id: str | None
    kind: DeliveryKind
    from_peer_id: str | None
    from_peer_name: str
    to_peer_name: str
    text: str
    created_at: str
    expires_at: str
    correlation_id: str | None = None
    attachments: list[dict[str, Any]] | None = None
    metadata: dict[str, Any] | None = None


class SQLiteQueuedDeliveryStore:
    """Durable, capped, delete-on-drain delivery queue."""

    def __init__(
        self,
        db: StateDatabase,
        *,
        ttl_seconds: float,
        max_per_peer: int,
    ) -> None:
        self._conn = db.conn
        self._ttl_seconds = max(0.0, ttl_seconds)
        self._max_per_peer = max(0, max_per_peer)

    def enqueue(
        self,
        *,
        peer_id: str,
        kind: DeliveryKind,
        from_peer_name: str,
        to_peer_name: str,
        text: str,
        repowire_session_id: str | None = None,
        from_peer_id: str | None = None,
        correlation_id: str | None = None,
        attachments: list[dict[str, Any]] | None = None,
        metadata: dict[str, Any] | None = None,
        now: datetime | None = None,
    ) -> QueuedDelivery | None:
        """Append one delivery and evict expired/over-cap rows for the peer."""
        if self._max_per_peer <= 0 or self._ttl_seconds <= 0:
            return None
        base = now or datetime.now(timezone.utc)
        if base.tzinfo is None:
            base = base.replace(tzinfo=timezone.utc)
        created_at = base.astimezone(timezone.utc).isoformat()
        expires_at = (
            base.astimezone(timezone.utc) + timedelta(seconds=self._ttl_seconds)
        ).isoformat()
        delivery = QueuedDelivery(
            delivery_id=f"qd-{uuid4().hex[:12]}",
            peer_id=peer_id,
            repowire_session_id=repowire_session_id,
            kind=kind,
            from_peer_id=from_peer_id,
            from_peer_name=from_peer_name,
            to_peer_name=to_peer_name,
            correlation_id=correlation_id,
            text=text,
            attachments=attachments or [],
            metadata=metadata or {},
            created_at=created_at,
            expires_at=expires_at,
        )
        with self._conn:
            self._delete_expired_locked(created_at)
            self._conn.execute(
                """
                INSERT INTO queued_deliveries(
                    delivery_id, peer_id, repowire_session_id, kind, from_peer_id,
                    from_peer_name, to_peer_name, correlation_id, text,
                    attachments_json, metadata_json, created_at, expires_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    delivery.delivery_id,
                    delivery.peer_id,
                    delivery.repowire_session_id,
                    delivery.kind,
                    delivery.from_peer_id,
                    delivery.from_peer_name,
                    delivery.to_peer_name,
                    delivery.correlation_id,
                    delivery.text,
                    json.dumps(delivery.attachments or []),
                    json.dumps(delivery.metadata or {}),
                    delivery.created_at,
                    delivery.expires_at,
                ),
            )
            self._enforce_cap_locked(peer_id)
        return delivery

    def drain_for_peer(
        self,
        peer_id: str,
        *,
        max_results: int = 50,
        now: datetime | None = None,
    ) -> list[QueuedDelivery]:
        """Return and delete unexpired deliveries for one peer, oldest first."""
        base = now or datetime.now(timezone.utc)
        if base.tzinfo is None:
            base = base.replace(tzinfo=timezone.utc)
        cutoff = base.astimezone(timezone.utc).isoformat()
        limit = max(0, max_results)
        if limit <= 0:
            return []
        with self._conn:
            self._delete_expired_locked(cutoff)
            rows = self._conn.execute(
                """
                SELECT * FROM queued_deliveries
                WHERE peer_id = ? AND expires_at > ?
                ORDER BY created_at ASC, delivery_id ASC
                LIMIT ?
                """,
                (peer_id, cutoff, limit),
            ).fetchall()
            ids = [row["delivery_id"] for row in rows]
            if ids:
                self._conn.executemany(
                    "DELETE FROM queued_deliveries WHERE delivery_id = ?",
                    [(delivery_id,) for delivery_id in ids],
                )
        return [self._row_to_delivery(row) for row in rows]

    def count_for_peer(self, peer_id: str) -> int:
        row = self._conn.execute(
            "SELECT COUNT(*) FROM queued_deliveries WHERE peer_id = ?",
            (peer_id,),
        ).fetchone()
        return int(row[0]) if row is not None else 0

    def _delete_expired_locked(self, cutoff_iso: str) -> None:
        self._conn.execute(
            "DELETE FROM queued_deliveries WHERE expires_at <= ?",
            (cutoff_iso,),
        )

    def _enforce_cap_locked(self, peer_id: str) -> None:
        rows = self._conn.execute(
            """
            SELECT delivery_id FROM queued_deliveries
            WHERE peer_id = ?
            ORDER BY created_at DESC, delivery_id DESC
            LIMIT -1 OFFSET ?
            """,
            (peer_id, self._max_per_peer),
        ).fetchall()
        if rows:
            self._conn.executemany(
                "DELETE FROM queued_deliveries WHERE delivery_id = ?",
                [(row["delivery_id"],) for row in rows],
            )

    @staticmethod
    def _row_to_delivery(row: sqlite3.Row) -> QueuedDelivery:
        attachments = json.loads(row["attachments_json"] or "[]")
        metadata = json.loads(row["metadata_json"] or "{}")
        return QueuedDelivery(
            delivery_id=row["delivery_id"],
            peer_id=row["peer_id"],
            repowire_session_id=row["repowire_session_id"],
            kind=row["kind"],
            from_peer_id=row["from_peer_id"],
            from_peer_name=row["from_peer_name"],
            to_peer_name=row["to_peer_name"],
            correlation_id=row["correlation_id"],
            text=row["text"],
            attachments=attachments if isinstance(attachments, list) else [],
            metadata=metadata if isinstance(metadata, dict) else {},
            created_at=row["created_at"],
            expires_at=row["expires_at"],
        )
