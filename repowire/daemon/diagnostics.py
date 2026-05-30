"""Peer diagnostics: contradiction detection and the ``peer doctor`` report.

This module is the single source of truth for the contradiction predicates
and inbound-health computation shared by:

- ``GET /peers/{id}/doctor`` (on-demand operator report)
- ``GET /peers`` / ``GET /peers/{id}`` (inbound_status field)
- ``lazy_repair`` reconciliation passes (auto-emitted fail-loud events)

It reuses existing read-only primitives (tmux pane probe, pane runtime
metadata, ask tracker, websocket transport) rather than introducing new
state. Local-only probes (tmux, hook metadata, agent pid) are gated on the
peer living on the same machine as the daemon; remote peers degrade
gracefully to ``unavailable`` rather than producing false contradictions.
"""

from __future__ import annotations

import asyncio
import logging
import os
import socket
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Protocol

from pydantic import BaseModel, Field

from repowire.hooks.utils import read_pane_runtime_metadata
from repowire.protocol.peers import Peer, PeerRole, PeerStatus, TurnState
from repowire.spawn_ownership import probe_tmux_pane

if TYPE_CHECKING:
    from repowire.daemon.ask_tracker import AskTracker

logger = logging.getLogger(__name__)

# An inbound ask still open past this age is surfaced as a (warning)
# contradiction: it likely means delivery or pickup is stuck.
STALE_PENDING_THRESHOLD_SECONDS = 1800.0


class _Transport(Protocol):
    """The slice of WebSocketTransport diagnostics needs (duck-typed for tests)."""

    def is_connected(self, session_id: str) -> bool: ...

    def get_connection_pane_id(self, session_id: str) -> str | None: ...


# --- contradiction codes -------------------------------------------------

ONLINE_BUT_NO_WS = "ONLINE_BUT_NO_WS"
PANE_MISSING = "PANE_MISSING"
AGENT_PID_DEAD = "AGENT_PID_DEAD"
HOOK_PEERID_MISMATCH = "HOOK_PEERID_MISMATCH"
WS_PANE_MISMATCH = "WS_PANE_MISMATCH"
STALE_PENDING_ASK = "STALE_PENDING_ASK"

SEVERITY_ERROR = "error"
SEVERITY_WARNING = "warning"

# --- inbound status ---

INBOUND_OFFLINE = "offline"
INBOUND_PANE_UNSAFE = "pane_unsafe"
INBOUND_NO_HOOK = "no_hook"
INBOUND_LEGACY_UNVERIFIED = "legacy_unverified"
INBOUND_DEGRADED = "inbound_degraded"
INBOUND_ONLINE = "online"


def compute_inbound_status(
    *,
    is_offline: bool,
    ws_connected: bool,
    hook_supports_receipts: bool,
    pane_safe: bool | None = None,
    last_success_at: str | None = None,
    last_failure_at: str | None = None,
) -> str:
    """Classify a peer's inbound-delivery reachability.

    Precedence (first match wins):
      offline -> pane_unsafe -> no_hook -> legacy_unverified -> inbound_degraded -> online

    - offline: the registry considers the peer down.
    - pane_unsafe: asserted ONLY when pane safety was actually probed and failed
      (pane_safe is False). pane_safe=None means "not probed" and is skipped.
    - no_hook: no live ws connection at all (no inbound hook reachable).
    - legacy_unverified: connected and can inject, but receipt capability is
      neither advertised nor observed -- the hook works but is unverified.
    - inbound_degraded: connected + receipt-capable, but the most recent
      injection failed more recently than it succeeded.
    - online: connected, receipt-capable, no fresher failure than success.
    """
    if is_offline:
        return INBOUND_OFFLINE
    if ws_connected and pane_safe is False:
        return INBOUND_PANE_UNSAFE
    if not ws_connected:
        return INBOUND_NO_HOOK
    if not hook_supports_receipts:
        return INBOUND_LEGACY_UNVERIFIED
    if last_failure_at is not None and (
        last_success_at is None or last_failure_at > last_success_at
    ):
        return INBOUND_DEGRADED
    return INBOUND_ONLINE


class Contradiction(BaseModel):
    """A detected inconsistency between what the registry claims and reality."""

    code: str
    severity: str  # "error" | "warning"
    detail: str


class DoctorReport(BaseModel):
    """Operator-facing diagnostic snapshot for a single peer."""

    # registry identity (source of truth)
    peer_id: str
    display_name: str
    status: str
    turn_state: TurnState | None = None
    last_seen: str | None = None
    machine: str | None = None
    backend: str = "claude-code"
    circle: str = "global"
    role: PeerRole = PeerRole.AGENT
    pane_id: str | None = None

    # inbound reachability (local-only signals degrade on remote peers)
    is_local_machine: bool = True
    ws_connected: bool = False
    ws_pane_id: str | None = None
    tmux_pane_exists: bool | None = None  # None == unavailable (remote / no pane_id)
    tmux_evidence: dict | None = None
    hook_meta_available: bool = False
    hook_meta_peer_id: str | None = None
    hook_meta_display_name: str | None = None
    agent_pid: int | None = None
    agent_pid_alive: bool | None = None  # None == unavailable

    # ask state
    pending_inbound_count: int = 0
    oldest_pending_age_seconds: float | None = None
    oldest_pending_cid: str | None = None

    # the payoff
    contradictions: list[Contradiction] = Field(default_factory=list)

    @property
    def has_errors(self) -> bool:
        return any(c.severity == SEVERITY_ERROR for c in self.contradictions)


# --- pure helpers ---------------------------------------------------------


def _agent_pid_alive(pid: int) -> bool:
    """Return whether ``pid`` is a live process (pid-specific, no tmux fallback)."""
    try:
        os.kill(pid, 0)
    except PermissionError:
        return True  # exists but owned by another user
    except OSError:
        return False
    return True


def is_local_machine(peer: Peer, *, hostname: str | None = None) -> bool:
    """Whether ``peer`` runs on the daemon's host (so local probes are valid).

    Exact hostname match is the strong signal. As a fallback, a peer whose
    machine is unknown/blank but which has a local pane id is treated as local:
    older WebSocket registrations recorded ``machine="unknown"``, and refusing
    to probe them would silently hide real local contradictions.
    """
    host = hostname or socket.gethostname()
    if peer.machine == host:
        return True
    if peer.machine in (None, "", "unknown") and peer.pane_id:
        return True
    return False


# --- report assembly ------------------------------------------------------


async def build_doctor_report(
    peer: Peer,
    transport: _Transport,
    ask_tracker: AskTracker,
    *,
    hostname: str | None = None,
) -> DoctorReport:
    """Assemble a DoctorReport for ``peer`` from read-only evidence.

    All local-machine probes are skipped (left ``None`` / unavailable) when the
    peer lives on a different machine than the daemon, so we never assert a
    contradiction we cannot actually verify locally.
    """
    host = hostname or socket.gethostname()
    local = is_local_machine(peer, hostname=host)
    status_value = peer.status.value

    ws_connected = transport.is_connected(peer.peer_id)
    ws_pane_id = transport.get_connection_pane_id(peer.peer_id) if ws_connected else None

    report = DoctorReport(
        peer_id=peer.peer_id,
        display_name=peer.display_name,
        status=status_value,
        turn_state=peer.turn_state,
        last_seen=peer.last_seen.isoformat() if peer.last_seen else None,
        machine=peer.machine,
        backend=str(peer.backend),
        circle=peer.circle,
        role=peer.role,
        pane_id=peer.pane_id,
        is_local_machine=local,
        ws_connected=ws_connected,
        ws_pane_id=ws_pane_id,
        agent_pid=peer.agent_pid,
    )

    # --- local-only evidence ---
    if local and peer.pane_id:
        # probe_tmux_pane shells out to tmux; offload so we don't block the loop.
        evidence = await asyncio.to_thread(probe_tmux_pane, peer.pane_id)
        report.tmux_pane_exists = evidence is not None
        if evidence is not None:
            report.tmux_evidence = {
                "tmux_session": evidence.tmux_session,
                "current_path": evidence.current_path,
                "pane_pid": evidence.pane_pid,
            }
    if local:
        meta = read_pane_runtime_metadata(peer.pane_id)
        if meta:
            report.hook_meta_available = True
            hook_peer_id = meta.get("peer_id")
            report.hook_meta_peer_id = hook_peer_id if isinstance(hook_peer_id, str) else None
            hook_name = meta.get("display_name")
            report.hook_meta_display_name = hook_name if isinstance(hook_name, str) else None
        if peer.agent_pid is not None:
            report.agent_pid_alive = _agent_pid_alive(peer.agent_pid)

    # --- ask state ---
    pending = await ask_tracker.pending_for_peer(peer.peer_id, max_results=50, direction="inbound")
    report.pending_inbound_count = len(pending)
    if pending:
        oldest = pending[-1]  # pending_for_peer is newest-first
        report.oldest_pending_cid = oldest.correlation_id
        report.oldest_pending_age_seconds = (
            datetime.now(timezone.utc) - oldest.created_at
        ).total_seconds()

    report.contradictions = detect_contradictions(peer, report)
    return report


def detect_contradictions(peer: Peer, report: DoctorReport) -> list[Contradiction]:
    """Compute contradictions from an assembled report. Pure; reused by Area 4."""
    found: list[Contradiction] = []
    local = report.is_local_machine

    # ONLINE_BUT_NO_WS: registry says live, but no websocket terminates here.
    if (
        local
        and peer.status in (PeerStatus.ONLINE, PeerStatus.BUSY)
        and not report.ws_connected
    ):
        found.append(
            Contradiction(
                code=ONLINE_BUT_NO_WS,
                severity=SEVERITY_ERROR,
                detail=f"peer is {report.status} but has no live WebSocket connection",
            )
        )

    # PANE_MISSING: registry records a pane that tmux no longer has.
    if local and peer.pane_id and report.tmux_pane_exists is False:
        found.append(
            Contradiction(
                code=PANE_MISSING,
                severity=SEVERITY_ERROR,
                detail=f"registry pane {peer.pane_id} is not present in tmux",
            )
        )

    # AGENT_PID_DEAD: the owning agent process is gone.
    if local and peer.agent_pid is not None and report.agent_pid_alive is False:
        found.append(
            Contradiction(
                code=AGENT_PID_DEAD,
                severity=SEVERITY_ERROR,
                detail=f"agent pid {peer.agent_pid} is not a live process",
            )
        )

    # HOOK_PEERID_MISMATCH: the live ws-hook in the pane claims a different peer.
    if (
        report.hook_meta_available
        and report.hook_meta_peer_id
        and report.hook_meta_peer_id != peer.peer_id
    ):
        found.append(
            Contradiction(
                code=HOOK_PEERID_MISMATCH,
                severity=SEVERITY_ERROR,
                detail=(
                    f"pane hook metadata peer_id {report.hook_meta_peer_id} "
                    f"differs from registry peer_id {peer.peer_id}"
                ),
            )
        )

    # WS_PANE_MISMATCH: the live socket is bound to a different pane than recorded.
    if (
        report.ws_connected
        and report.ws_pane_id
        and peer.pane_id
        and report.ws_pane_id != peer.pane_id
    ):
        found.append(
            Contradiction(
                code=WS_PANE_MISMATCH,
                severity=SEVERITY_WARNING,
                detail=(
                    f"websocket is bound to pane {report.ws_pane_id} "
                    f"but registry records pane {peer.pane_id}"
                ),
            )
        )

    # STALE_PENDING_ASK: an inbound ask has been open too long.
    if (
        report.oldest_pending_age_seconds is not None
        and report.oldest_pending_age_seconds > STALE_PENDING_THRESHOLD_SECONDS
    ):
        age_min = int(report.oldest_pending_age_seconds // 60)
        found.append(
            Contradiction(
                code=STALE_PENDING_ASK,
                severity=SEVERITY_WARNING,
                detail=(
                    f"oldest pending inbound ask {report.oldest_pending_cid} "
                    f"has been open ~{age_min}m"
                ),
            )
        )

    return found
