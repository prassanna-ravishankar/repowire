"""Lifetime cache for broker-side ACP clients.

One ``AcpClient`` per ACP-routed peer, lazily started on first use and torn
down on shutdown. Keyed by ``peer_id`` so display-name reuse across circles
doesn't collide.

Phase-3 scope: ``ask`` only. Notify, broadcast, etc. continue to flow over the
WS path even when the experiments flag is on.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from repowire.acp.client import AcpClient, AcpClientError, AcpPromptResult
from repowire.acp.models import AcpPeerConfig
from repowire.acp.permissions import ApprovalBroker

logger = logging.getLogger(__name__)


@dataclass
class AcpPeerSpec:
    """Resolved spec for spawning an ACP client on behalf of a peer."""

    peer_id: str
    config: AcpPeerConfig
    fallback_cwd: str | None = None


class AcpClientManager:
    """Holds one ``AcpClient`` per ACP-routed peer.

    Clients are started lazily on first ``prompt`` so registering a peer is
    cheap even if no ask ever flows through ACP. ``close`` is idempotent and
    safe to call from the daemon lifespan shutdown hook.
    """

    def __init__(self, *, approval_broker: ApprovalBroker | None = None) -> None:
        self._clients: dict[str, AcpClient] = {}
        self._lock = asyncio.Lock()
        self._closed = False
        self._approval_broker = approval_broker
        self._in_flight = 0
        self._last_error: str | None = None
        self._last_error_peer_id: str | None = None
        self._last_error_at: str | None = None
        self._last_success_at: str | None = None

    async def get_or_create(self, spec: AcpPeerSpec) -> AcpClient:
        """Return the live ``AcpClient`` for ``spec.peer_id``, starting it if needed."""
        if self._closed:
            raise AcpClientError("AcpClientManager is closed")
        async with self._lock:
            client = self._clients.get(spec.peer_id)
            if client is None:
                client = AcpClient(
                    spec.config,
                    fallback_cwd=spec.fallback_cwd,
                    peer_id=spec.peer_id,
                    approval_broker=self._approval_broker,
                )
                self._clients[spec.peer_id] = client
        return client

    async def prompt(
        self,
        spec: AcpPeerSpec,
        text: str,
        *,
        timeout: float = 120.0,
    ) -> AcpPromptResult:
        """Route one ask to its ACP-routed peer and return the assistant turn.

        On any ``AcpClientError`` (timeout, transport failure, protocol error)
        the cached client is dropped so the *next* ask for this peer respawns
        a fresh subprocess. Without this, one crash would poison the cache:
        ``AcpClient.prompt`` already closes itself on failure, so a retained
        cache entry would just be a corpse.
        """
        client = await self.get_or_create(spec)
        self._in_flight += 1
        try:
            result = await client.prompt(text, timeout=timeout)
        except AcpClientError as e:
            self._record_error(spec.peer_id, e)
            await self._drop_if_crashed(spec.peer_id, client)
            raise
        finally:
            self._in_flight = max(0, self._in_flight - 1)
        # Belt-and-braces: if the client decided it's crashed after a "successful"
        # call (shouldn't happen today, but cheap to guard), evict it too.
        if client.crashed:
            await self._drop_if_crashed(spec.peer_id, client)
        self._last_error = None
        self._last_error_peer_id = None
        self._last_error_at = None
        self._last_success_at = _utc_now()
        return result

    def _record_error(self, peer_id: str, error: BaseException | None = None) -> None:
        self._last_error = str(error) if error is not None else "ACP prompt failed"
        self._last_error_peer_id = peer_id
        self._last_error_at = _utc_now()

    def health_snapshot(self) -> dict[str, Any]:
        """Return passive ACP broker state for /health and doctor.

        This intentionally does not spawn clients or probe subprocesses; it only
        reports what the broker already knows from prior delivery attempts.
        """
        return {
            "manager_initialized": True,
            "closed": self._closed,
            "active_clients": len(self._clients),
            "in_flight": self._in_flight,
            "last_error": self._last_error,
            "last_error_peer_id": self._last_error_peer_id,
            "last_error_at": self._last_error_at,
            "last_success_at": self._last_success_at,
            # Broker-protocol session ids, exposed for diagnostics/timeline only.
            # NOT a backend resume handle — do not feed into runtime_session_id.
            "sessions": {
                peer_id: client.session_id
                for peer_id, client in self._clients.items()
                if client.session_id is not None
            },
        }

    async def _drop_if_crashed(self, peer_id: str, client: AcpClient) -> None:
        """Drop ``peer_id`` from cache iff the cached entry is ``client``.

        Guards against races where two prompts targeted the same peer, one
        crashed, the other already started a fresh respawn — we don't want to
        evict the fresh one.
        """
        async with self._lock:
            if self._clients.get(peer_id) is client:
                self._clients.pop(peer_id, None)
        await client.close()

    async def drop(self, peer_id: str) -> None:
        """Tear down the client for ``peer_id`` if any. Used on peer eviction."""
        async with self._lock:
            client = self._clients.pop(peer_id, None)
        if client is not None:
            await client.close()

    async def cancel_existing(self, peer_id: str) -> dict[str, Any]:
        """Best-effort ``session/cancel`` for an already-live ACP client.

        This never starts a subprocess. It is used by tracked-work cancel to
        honor protocol-cancel ordering only when the daemon already has a live
        ACP session for the assigned peer.
        """
        async with self._lock:
            client = self._clients.get(peer_id)
        if client is None:
            return {
                "attempted": False,
                "status": "unavailable",
                "reason": "no_active_acp_client",
                "peer_id": peer_id,
            }
        await client.cancel()
        return {
            "attempted": True,
            "status": "sent",
            "reason": "session_cancel_sent",
            "peer_id": peer_id,
        }

    async def close(self) -> None:
        """Tear down every live client. Safe to call multiple times."""
        if self._closed:
            return
        self._closed = True
        async with self._lock:
            clients = list(self._clients.values())
            self._clients.clear()
        for c in clients:
            try:
                await c.close()
            except Exception as e:  # noqa: BLE001 — best-effort shutdown
                logger.warning("AcpClientManager: close error: %s", e)


def _utc_now() -> str:
    return datetime.now(UTC).isoformat()
