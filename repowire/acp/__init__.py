"""Broker-side ACP (Agent Client Protocol) client.

Phase-3 of the ACP migration (see notes-vault/repowire-mesh/acp-mapping-delta-claude.md).
Lets the broker act as an ACP client against an ACP-speaking agent subprocess
(e.g. `codex-acp`) instead of routing asks through the WebSocket / tmux-paste path.

Gated by `experiments.acp_broker_client` in config; only peers whose registry
metadata carries an `acp` block are routed through this client. All other peers
keep using the existing wire transport unchanged.

Built on the official `agent-client-protocol` python SDK — this package owns
the broker-side lifecycle and a small notification recorder, not JSON-RPC plumbing.
"""

from repowire.acp.broker import (
    AcpAckCallback,
    AcpRouteDecision,
    decide_acp_route,
    deliver_ask_via_acp,
    maybe_decide_acp_route,
)
from repowire.acp.client import AcpClient, AcpClientError, AcpPromptResult
from repowire.acp.manager import AcpClientManager, AcpPeerSpec
from repowire.acp.models import AcpPeerConfig

__all__ = [
    "AcpAckCallback",
    "AcpClient",
    "AcpClientError",
    "AcpClientManager",
    "AcpPeerConfig",
    "AcpPeerSpec",
    "AcpPromptResult",
    "AcpRouteDecision",
    "decide_acp_route",
    "deliver_ask_via_acp",
    "maybe_decide_acp_route",
]
