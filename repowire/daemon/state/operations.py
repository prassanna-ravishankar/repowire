"""SQLite-backed durable operation lifecycle records."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any
from uuid import uuid4

from repowire.daemon.state.database import StateDatabase
from repowire.daemon.work_store import json_dumps, json_loads, now_iso


def new_operation_id() -> str:
    return f"op-{uuid4().hex[:12]}"


@dataclass(frozen=True)
class Operation:
    operation_id: str
    kind: str
    state: str
    target: dict[str, Any] = field(default_factory=dict)
    strategy: str | None = None
    attempts: list[dict[str, Any]] = field(default_factory=list)
    result: dict[str, Any] = field(default_factory=dict)
    error: dict[str, Any] = field(default_factory=dict)
    provenance: dict[str, Any] = field(default_factory=dict)
    created_at: str = ""
    updated_at: str = ""
    completed_at: str | None = None

    def status(self) -> dict[str, Any]:
        return {
            "operation_id": self.operation_id,
            "kind": self.kind,
            "state": self.state,
            "target": self.target,
            "strategy": self.strategy,
            "attempts": self.attempts,
            "result": self.result,
            "error": self.error,
            "provenance": self.provenance,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "completed_at": self.completed_at,
        }


class SQLiteOperationStore:
    """Repository for internal command/operation audit state."""

    def __init__(self, db: StateDatabase) -> None:
        self._conn = db.conn

    @staticmethod
    def _row_to_operation(row) -> Operation | None:
        if row is None:
            return None
        return Operation(
            operation_id=row["operation_id"],
            kind=row["kind"],
            state=row["state"],
            target=json_loads(row["target_json"], {}),
            strategy=row["strategy"],
            attempts=json_loads(row["attempts_json"], []),
            result=json_loads(row["result_json"], {}),
            error=json_loads(row["error_json"], {}),
            provenance=json_loads(row["provenance_json"], {}),
            created_at=row["created_at"],
            updated_at=row["updated_at"],
            completed_at=row["completed_at"],
        )

    def create(
        self,
        *,
        kind: str,
        target: dict[str, Any] | None = None,
        provenance: dict[str, Any] | None = None,
    ) -> Operation:
        now = now_iso()
        operation = Operation(
            operation_id=new_operation_id(),
            kind=kind,
            state="queued",
            target=target or {},
            provenance=provenance or {},
            created_at=now,
            updated_at=now,
        )
        with self._conn:
            self._conn.execute(
                """
                INSERT INTO operations(
                    operation_id, kind, state, target_json, strategy,
                    attempts_json, result_json, error_json, provenance_json,
                    created_at, updated_at, completed_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    operation.operation_id,
                    operation.kind,
                    operation.state,
                    json_dumps(operation.target),
                    operation.strategy,
                    json_dumps(operation.attempts),
                    json_dumps(operation.result),
                    json_dumps(operation.error),
                    json_dumps(operation.provenance),
                    operation.created_at,
                    operation.updated_at,
                    operation.completed_at,
                ),
            )
        return operation

    def get(self, operation_id: str) -> Operation | None:
        row = self._conn.execute(
            "SELECT * FROM operations WHERE operation_id = ?",
            (operation_id,),
        ).fetchone()
        return self._row_to_operation(row)

    def list_all(self, *, kind: str | None = None, state: str | None = None) -> list[Operation]:
        clauses: list[str] = []
        params: list[str] = []
        if kind is not None:
            clauses.append("kind = ?")
            params.append(kind)
        if state is not None:
            clauses.append("state = ?")
            params.append(state)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        rows = self._conn.execute(
            f"SELECT * FROM operations {where} ORDER BY updated_at DESC",
            params,
        ).fetchall()
        return [
            operation
            for row in rows
            if (operation := self._row_to_operation(row)) is not None
        ]

    def start_attempt(
        self,
        operation_id: str,
        *,
        strategy: str | None = None,
        detail: dict[str, Any] | None = None,
    ) -> Operation | None:
        operation = self.get(operation_id)
        if operation is None:
            return None
        attempts = list(operation.attempts)
        attempts.append(
            {
                "attempt_id": f"op-attempt-{uuid4().hex[:12]}",
                "state": "running",
                "strategy": strategy,
                "detail": detail or {},
                "started_at": now_iso(),
                "completed_at": None,
                "error": {},
            }
        )
        return self._update(
            operation,
            state="running",
            strategy=strategy if strategy is not None else operation.strategy,
            attempts=attempts,
        )

    def complete(
        self,
        operation_id: str,
        *,
        strategy: str | None = None,
        result: dict[str, Any] | None = None,
    ) -> Operation | None:
        operation = self.get(operation_id)
        if operation is None:
            return None
        now = now_iso()
        attempts = list(operation.attempts)
        if attempts:
            attempts[-1] = {
                **attempts[-1],
                "state": "completed",
                "completed_at": now,
                "result": result or {},
            }
        return self._update(
            operation,
            state="completed",
            strategy=strategy if strategy is not None else operation.strategy,
            attempts=attempts,
            result=result or {},
            completed_at=now,
        )

    def fail(
        self,
        operation_id: str,
        *,
        state: str = "failed",
        strategy: str | None = None,
        error: dict[str, Any] | None = None,
    ) -> Operation | None:
        operation = self.get(operation_id)
        if operation is None:
            return None
        now = now_iso()
        attempts = list(operation.attempts)
        if attempts:
            attempts[-1] = {
                **attempts[-1],
                "state": state,
                "completed_at": now,
                "error": error or {},
            }
        return self._update(
            operation,
            state=state,
            strategy=strategy if strategy is not None else operation.strategy,
            attempts=attempts,
            error=error or {},
            completed_at=now,
        )

    def _update(
        self,
        operation: Operation,
        *,
        state: str,
        strategy: str | None,
        attempts: list[dict[str, Any]] | None = None,
        result: dict[str, Any] | None = None,
        error: dict[str, Any] | None = None,
        completed_at: str | None = None,
    ) -> Operation | None:
        now = now_iso()
        with self._conn:
            self._conn.execute(
                """
                UPDATE operations
                SET state = ?, strategy = ?, attempts_json = ?, result_json = ?,
                    error_json = ?, updated_at = ?, completed_at = ?
                WHERE operation_id = ?
                """,
                (
                    state,
                    strategy,
                    json_dumps(attempts if attempts is not None else operation.attempts),
                    json_dumps(result if result is not None else operation.result),
                    json_dumps(error if error is not None else operation.error),
                    now,
                    completed_at,
                    operation.operation_id,
                ),
            )
        return self.get(operation.operation_id)
