"""Glue between repowire's broker (ask/ack pipeline) and the ACP client.

When the ``experiments.acp_broker_client`` flag is on and the target peer
carries an ``acp`` metadata block, an incoming ``/ask`` is routed here instead
of through the WebSocket transport.

Flow:

    /ask (asker, ACP-peer)
        ─► register in ask_tracker (same as today)
        ─► ACP: session/prompt against codex-acp subprocess
        ─► on completion: notify asker with the assembled reply + close ask

Phase-3 keeps everything else on the WS path (notify, broadcast, ack outbound
from non-ACP peers, …). The session is persistent per peer, so successive
asks to the same ACP peer reuse the same ACP session.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from pydantic import ValidationError

from repowire.acp.manager import AcpClientManager, AcpPeerSpec
from repowire.acp.models import AcpPeerConfig
from repowire.protocol.peers import Peer

logger = logging.getLogger(__name__)

# Default per-prompt timeout; matches DEFAULT_QUERY_TIMEOUT roughly but capped
# lower so a single hung codex-acp turn doesn't pin an ask thread for 5 min.
DEFAULT_PROMPT_TIMEOUT = 180.0


@dataclass
class AcpRouteDecision:
    """Outcome of "should this ask go via ACP?".

    ``spec`` is set iff routing should happen. ``reason`` is human-readable.
    """

    route: bool
    spec: AcpPeerSpec | None = None
    reason: str = ""


def decide_acp_route(peer: Peer, *, flag_enabled: bool) -> AcpRouteDecision:
    """Inspect a peer + the experiments flag to decide on ACP routing.

    Routes if and only if:
      * ``experiments.acp_broker_client`` is on, AND
      * peer carries a well-formed ``metadata["acp"]`` block.

    Bad ``metadata["acp"]`` content (wrong shape) is logged and treated as
    "do not route" — the caller falls back to the WS path.
    """
    if not flag_enabled:
        return AcpRouteDecision(route=False, reason="experiments.acp_broker_client off")
    raw = peer.metadata.get("acp") if peer.metadata else None
    if not raw:
        return AcpRouteDecision(route=False, reason="peer has no acp metadata block")
    try:
        config = AcpPeerConfig.model_validate(raw)
    except ValidationError as e:
        logger.warning(
            "ACP route declined for peer_id=%s: invalid metadata.acp block: %s",
            peer.peer_id, e,
        )
        return AcpRouteDecision(route=False, reason=f"invalid acp metadata: {e}")
    spec = AcpPeerSpec(peer_id=peer.peer_id, config=config, fallback_cwd=peer.path)
    return AcpRouteDecision(route=True, spec=spec, reason="acp routing engaged")


AcpAckCallback = Callable[[str, str | None, str | None], Awaitable[None]]
"""Callback fired when an ACP-routed ask completes.

Args: (correlation_id, reply_text_or_None, error_or_None).
- reply_text non-None: successful turn; deliver as ack reply.
- error non-None: ACP path failed; close the ask with an error frame.
"""


async def deliver_ask_via_acp(
    *,
    manager: AcpClientManager,
    spec: AcpPeerSpec,
    correlation_id: str,
    text: str,
    on_complete: AcpAckCallback,
    timeout: float = DEFAULT_PROMPT_TIMEOUT,
) -> asyncio.Task[None]:
    """Spawn a background task that prompts the ACP peer and acks the asker.

    Returned task is not awaited here; the ``/ask`` handler still returns the
    correlation_id immediately (matching the existing non-blocking semantics).
    The task acks via ``on_complete`` regardless of success or failure so an
    open ask never silently dies.
    """

    async def _run() -> None:
        try:
            result = await manager.prompt(spec, text, timeout=timeout)
            reply = result.text or f"[acp stop_reason={result.stop_reason}, no text]"
            await on_complete(correlation_id, reply, None)
        except Exception as e:  # noqa: BLE001 — surface every failure to the asker
            logger.exception(
                "ACP ask delivery failed for cid=%s peer_id=%s: %s",
                correlation_id, spec.peer_id, e,
            )
            await on_complete(correlation_id, None, str(e))

    return asyncio.create_task(_run(), name=f"acp-ask-{correlation_id[:8]}")
