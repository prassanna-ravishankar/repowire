from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any, Callable, Coroutine


class Blackboard:
    def __init__(self, persist_path: Path | None = None) -> None:
        self._data: dict[str, Any] = {}
        self._persist_path = persist_path
        self._subscribers: list[Callable[[str, Any, str], Coroutine[Any, Any, None]]] = []
        self._lock = asyncio.Lock()

        if persist_path and persist_path.exists():
            self._load()

    def _load(self) -> None:
        if self._persist_path and self._persist_path.exists():
            with open(self._persist_path) as f:
                self._data = json.load(f)

    def _save(self) -> None:
        if self._persist_path:
            self._persist_path.parent.mkdir(parents=True, exist_ok=True)
            with open(self._persist_path, "w") as f:
                json.dump(self._data, f, indent=2)

    async def write(self, key: str, value: Any, source_agent: str) -> None:
        async with self._lock:
            self._data[key] = value
            self._save()

        for subscriber in self._subscribers:
            asyncio.create_task(subscriber(key, value, source_agent))

    async def read(self, key: str) -> Any:
        async with self._lock:
            return self._data.get(key)

    async def read_all(self) -> dict[str, Any]:
        async with self._lock:
            return self._data.copy()

    async def delete(self, key: str) -> bool:
        async with self._lock:
            if key in self._data:
                del self._data[key]
                self._save()
                return True
            return False

    def subscribe(self, callback: Callable[[str, Any, str], Coroutine[Any, Any, None]]) -> None:
        self._subscribers.append(callback)

    def unsubscribe(self, callback: Callable[[str, Any, str], Coroutine[Any, Any, None]]) -> None:
        if callback in self._subscribers:
            self._subscribers.remove(callback)
