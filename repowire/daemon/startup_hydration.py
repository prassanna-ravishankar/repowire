"""One-shot startup rehydration for pane-backed peers.

The daemon's live peer registry is intentionally in-memory. On daemon restart,
idle sessions can still be alive in tmux while their ws-hook is gone, leaving
them invisible until a hook/MCP path re-announces them. This module repairs
only the cases where durable identity and live runtime evidence agree.
"""

from __future__ import annotations

import asyncio
import logging
import os
from dataclasses import dataclass
from typing import Any

from repowire.agent_types import AgentType
from repowire.daemon.peer_registry import PeerRegistry
from repowire.daemon.registry_identity import SessionMapping
from repowire.daemon.registry_repair import pid_alive
from repowire.daemon.state.session_bindings import SQLiteSessionBindingStore
from repowire.daemon.websocket_transport import WebSocketTransport
from repowire.hooks._tmux import PaneInfo, list_all_panes
from repowire.hooks.utils import read_pane_runtime_metadata
from repowire.protocol.peers import PeerStatus

logger = logging.getLogger(__name__)

DEFAULT_CONNECT_WAIT_SECONDS = 2.0
DEFAULT_RESPAWN_LIMIT = 20


@dataclass(frozen=True)
class StartupHydrationCandidate:
    peer_id: str
    display_name: str
    circle: str
    backend: AgentType
    path: str
    role: Any
    model: str | None
    pane_id: str
    tmux_session: str | None
    agent_pid: int
    parent_pid: int | None
    metadata: dict[str, Any]
    cert_nonce: str | None = None


@dataclass(frozen=True)
class StartupHydrationResult:
    hydrated: int = 0
    respawned: int = 0
    connected: int = 0
    no_transport: int = 0
    skipped: int = 0


def _normalize_path(path: str | None) -> str:
    if not path:
        return ""
    return os.path.normpath(os.path.abspath(path))


def _int_value(value: Any) -> int | None:
    if isinstance(value, int) and value > 0:
        return value
    if isinstance(value, str) and value.isdigit():
        parsed = int(value)
        return parsed if parsed > 0 else None
    return None


def _contradicts(values: list[int | None]) -> bool:
    concrete = {value for value in values if value is not None}
    return len(concrete) > 1


def _mapping_for_metadata(
    *,
    mappings: dict[str, SessionMapping],
    metadata: dict[str, Any],
    binding_store: SQLiteSessionBindingStore | None,
    pane: PaneInfo,
) -> tuple[SessionMapping | None, str | None, str | None]:
    """Return (mapping, cert_nonce, skip_reason) for strict peer_id/cert proof."""
    cert_nonce: str | None = None
    raw_peer_id = metadata.get("peer_id")
    peer_id = raw_peer_id if isinstance(raw_peer_id, str) and raw_peer_id else None
    cert_envelope = metadata.get("birth_certificate")
    cert_peer_id: str | None = None

    if cert_envelope is not None:
        if not isinstance(cert_envelope, dict):
            return None, None, "invalid_certificate"
        if binding_store is None:
            return None, None, "certificate_store_unavailable"
        cert_backend = cert_envelope.get("backend")
        cert_path = cert_envelope.get("project_path")
        cert_agent_pid = _int_value(cert_envelope.get("agent_pid"))
        if not isinstance(cert_backend, str) or not isinstance(cert_path, str):
            return None, None, "invalid_certificate"
        cert = binding_store.validate_birth_certificate(
            cert_envelope,
            backend=cert_backend,
            project_path=cert_path,
            pane_id=pane["pane_id"],
            agent_pid=cert_agent_pid,
        )
        if cert is None:
            return None, None, "invalid_certificate"
        cert_peer_id = cert.peer_id
        cert_nonce = cert.nonce

    if peer_id and cert_peer_id and peer_id != cert_peer_id:
        return None, None, "peer_id_certificate_mismatch"

    identity_peer_id = peer_id or cert_peer_id
    if not identity_peer_id:
        return None, None, "missing_peer_id"

    mapping = mappings.get(identity_peer_id)
    if mapping is None:
        return None, None, "mapping_missing"
    return mapping, cert_nonce, None


def _candidate_from_pane(
    *,
    pane: PaneInfo,
    metadata: dict[str, Any],
    mapping: SessionMapping,
    cert_nonce: str | None,
) -> tuple[StartupHydrationCandidate | None, str | None]:
    metadata_backend = metadata.get("backend")
    if metadata_backend != mapping.backend.value:
        return None, "backend_mismatch"

    raw_metadata_path = metadata.get("cwd")
    metadata_path = _normalize_path(
        raw_metadata_path if isinstance(raw_metadata_path, str) else None
    )
    mapping_path = _normalize_path(mapping.path)
    pane_path = _normalize_path(pane["cwd"])
    if not metadata_path or not mapping_path:
        return None, "path_missing"
    if metadata_path != mapping_path or pane_path != mapping_path:
        return None, "path_mismatch"

    cert = metadata.get("birth_certificate")
    cert_agent_pid = (
        _int_value(cert.get("agent_pid")) if isinstance(cert, dict) else None
    )
    agent_pid = _int_value(metadata.get("agent_pid"))
    mapping_agent_pid = _int_value(mapping.agent_pid)
    if _contradicts([agent_pid, cert_agent_pid, mapping_agent_pid]):
        return None, "agent_pid_mismatch"
    effective_agent_pid = agent_pid or cert_agent_pid or mapping_agent_pid
    if effective_agent_pid is None:
        return None, "agent_pid_missing"
    if not pid_alive(effective_agent_pid):
        return None, "agent_pid_dead"

    display_name = metadata.get("display_name")
    if display_name is not None and display_name != mapping.display_name:
        return None, "display_name_mismatch"

    parent_pid = _int_value(metadata.get("parent_pid"))
    tmux_session = f"{pane['session']}:{pane['window']}" if pane.get("session") else None
    hydrated_metadata: dict[str, Any] = {
        "startup_hydration": True,
        "hydration_source": "startup_hydration",
    }
    hook_session_id = metadata.get("hook_session_id")
    if isinstance(hook_session_id, str) and hook_session_id:
        hydrated_metadata["hook_session_id"] = hook_session_id
    if cert_nonce:
        hydrated_metadata["birth_certificate_nonce"] = cert_nonce

    return (
        StartupHydrationCandidate(
            peer_id=mapping.session_id,
            display_name=mapping.display_name,
            circle=mapping.circle,
            backend=mapping.backend,
            path=mapping_path,
            role=mapping.role,
            model=mapping.model,
            pane_id=pane["pane_id"],
            tmux_session=tmux_session,
            agent_pid=effective_agent_pid,
            parent_pid=parent_pid,
            metadata=hydrated_metadata,
            cert_nonce=cert_nonce,
        ),
        None,
    )


def _build_candidates(
    *,
    registry: PeerRegistry,
    binding_store: SQLiteSessionBindingStore | None,
    panes: list[PaneInfo],
) -> tuple[list[StartupHydrationCandidate], list[tuple[str | None, str]]]:
    candidates: list[StartupHydrationCandidate] = []
    skipped: list[tuple[str | None, str]] = []
    by_peer_id: dict[str, list[StartupHydrationCandidate]] = {}

    mappings = dict(registry._mappings)
    for pane in panes:
        pane_id = pane["pane_id"]
        metadata = read_pane_runtime_metadata(pane_id)
        if not metadata:
            skipped.append((pane_id, "metadata_missing"))
            continue
        mapping, cert_nonce, reason = _mapping_for_metadata(
            mappings=mappings,
            metadata=metadata,
            binding_store=binding_store,
            pane=pane,
        )
        if reason:
            skipped.append((pane_id, reason))
            continue
        assert mapping is not None
        candidate, reason = _candidate_from_pane(
            pane=pane,
            metadata=metadata,
            mapping=mapping,
            cert_nonce=cert_nonce,
        )
        if reason:
            skipped.append((pane_id, reason))
            continue
        assert candidate is not None
        by_peer_id.setdefault(candidate.peer_id, []).append(candidate)

    for peer_id, peer_candidates in by_peer_id.items():
        if len(peer_candidates) > 1:
            for candidate in peer_candidates:
                skipped.append((candidate.pane_id, "duplicate_peer_claim"))
            logger.warning(
                "startup hydration skipped %s: %d panes claimed the same peer_id",
                peer_id,
                len(peer_candidates),
            )
            continue
        candidates.append(peer_candidates[0])
    return candidates, skipped


async def _wait_connected(
    transport: WebSocketTransport,
    peer_id: str,
    *,
    timeout_seconds: float,
) -> bool:
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while True:
        if transport.is_connected(peer_id):
            return True
        if asyncio.get_running_loop().time() >= deadline:
            return False
        await asyncio.sleep(0.05)


async def hydrate_startup_peers(
    *,
    registry: PeerRegistry,
    transport: WebSocketTransport,
    binding_store: SQLiteSessionBindingStore | None,
    connect_wait_seconds: float = DEFAULT_CONNECT_WAIT_SECONDS,
    respawn_limit: int = DEFAULT_RESPAWN_LIMIT,
) -> StartupHydrationResult:
    """Recreate strictly proven pane-backed peers after daemon startup.

    Candidates are anchored by persisted peer mappings and require daemon-minted
    pane metadata naming the same peer_id or a validated runtime certificate.
    Unproven panes are deliberately left for /panes/orphans and manual link.
    """
    panes = list_all_panes()
    candidates, skipped = _build_candidates(
        registry=registry,
        binding_store=binding_store,
        panes=panes,
    )
    for pane_id, reason in skipped:
        registry.add_event(
            "startup_hydration_skipped",
            {"pane_id": pane_id, "reason": reason},
        )

    if not candidates:
        return StartupHydrationResult(skipped=len(skipped))

    from repowire.hooks.ws_hook_supervisor import startup_respawn_ws_hook

    hydrated = 0
    respawned = 0
    connected = 0
    no_transport = 0
    pending_transport: list[StartupHydrationCandidate] = []

    for candidate in candidates:
        existing = await registry.get_peer(candidate.peer_id)
        if (
            existing is not None
            and existing.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
        ):
            if transport.is_connected(candidate.peer_id):
                connected += 1
            continue

        await registry.allocate_and_register(
            circle=candidate.circle,
            backend=candidate.backend,
            model=candidate.model,
            path=candidate.path,
            pane_id=candidate.pane_id,
            tmux_session=candidate.tmux_session,
            metadata=candidate.metadata,
            role=candidate.role,
            peer_id=candidate.peer_id,
            initial_status=PeerStatus.OFFLINE,
            agent_pid=candidate.agent_pid,
            parent_pid=candidate.parent_pid,
            circle_source="fallback",
            display_name_override=candidate.display_name,
        )
        hydrated += 1

        if transport.is_connected(candidate.peer_id):
            connected += 1
            continue

        if respawned < respawn_limit:
            did_respawn = await asyncio.to_thread(
                startup_respawn_ws_hook,
                candidate.pane_id,
                peer_id=candidate.peer_id,
                display_name=candidate.display_name,
                backend=candidate.backend.value,
                cwd=candidate.path,
                agent_pid=candidate.agent_pid,
            )
            if did_respawn:
                respawned += 1

        pending_transport.append(candidate)

    if pending_transport:
        wait_results = await asyncio.gather(
            *(
                _wait_connected(
                    transport,
                    candidate.peer_id,
                    timeout_seconds=connect_wait_seconds,
                )
                for candidate in pending_transport
            )
        )
        for candidate, did_connect in zip(
            pending_transport,
            wait_results,
            strict=True,
        ):
            if did_connect:
                connected += 1
                continue

            no_transport += 1
            registry.add_event(
                "startup_hydration_no_transport",
                {
                    "peer_id": candidate.peer_id,
                    "display_name": candidate.display_name,
                    "pane_id": candidate.pane_id,
                    "agent_pid": candidate.agent_pid,
                },
            )

    return StartupHydrationResult(
        hydrated=hydrated,
        respawned=respawned,
        connected=connected,
        no_transport=no_transport,
        skipped=len(skipped),
    )
