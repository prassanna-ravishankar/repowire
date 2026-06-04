"""Event helpers owned by the peer registry."""

from __future__ import annotations

import logging
from collections.abc import Callable
from typing import Any

from repowire.protocol.peers import Peer

logger = logging.getLogger(__name__)


class PeerContradictionTracker:
    """Transition-only dedup for fail-loud peer contradiction events."""

    def __init__(self) -> None:
        self._emitted: set[tuple[str, str]] = set()

    def emit(
        self,
        peer: Peer,
        code: str,
        severity: str,
        detail: str,
        add_event: Callable[[str, dict[str, Any]], str],
    ) -> None:
        """Emit ``peer_contradiction`` once per peer/code transition."""
        key = (peer.peer_id, code)
        if key in self._emitted:
            return
        self._emitted.add(key)
        try:
            add_event(
                "peer_contradiction",
                {
                    "code": code,
                    "severity": severity,
                    "peer_id": peer.peer_id,
                    "peer_name": peer.display_name,
                    "detail": detail,
                },
            )
        except Exception:  # noqa: BLE001 - reconciliation must not break on event errors
            logger.warning("Failed to emit peer_contradiction %s", code, exc_info=True)

    def clear(self, peer_id: str, code: str) -> None:
        """Forget one contradiction so a future recurrence re-emits once."""
        self._emitted.discard((peer_id, code))

    def clear_all(self, peer_id: str) -> None:
        """Drop all contradiction state for a peer."""
        self._emitted = {
            (pid, code) for (pid, code) in self._emitted if pid != peer_id
        }
