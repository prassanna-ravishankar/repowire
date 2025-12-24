from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any


class SharedState:
    def __init__(self, persist_path: Path | None = None) -> None:
        self._data: dict[str, Any] = {}
        self._persist_path = persist_path or Path.home() / ".repowire" / "state.json"
        self._lock = asyncio.Lock()
        self._load()

    def _load(self) -> None:
        if self._persist_path.exists():
            with open(self._persist_path) as f:
                self._data = json.load(f)

    def _save(self) -> None:
        self._persist_path.parent.mkdir(parents=True, exist_ok=True)
        with open(self._persist_path, "w") as f:
            json.dump(self._data, f, indent=2)

    async def write(self, key: str, value: Any) -> None:
        async with self._lock:
            self._data[key] = value
            self._save()

    async def read(self, key: str | None = None) -> Any:
        async with self._lock:
            if key is None:
                return self._data.copy()
            return self._data.get(key)

    async def delete(self, key: str) -> bool:
        async with self._lock:
            if key in self._data:
                del self._data[key]
                self._save()
                return True
            return False

    async def clear(self) -> None:
        async with self._lock:
            self._data = {}
            self._save()
