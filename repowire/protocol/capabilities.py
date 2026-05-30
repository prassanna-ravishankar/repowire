"""Hook capability advertisement.

Peers advertise a ``hook_version`` and a list of ``capabilities`` so the
daemon can tell, per peer, whether the inbound hook supports modern
behaviors (e.g. delivery receipts) rather than guessing from liveness alone.

Bump ``CURRENT_HOOK_VERSION`` whenever the hook<->daemon wire contract
changes in a way the daemon needs to branch on.
"""

from __future__ import annotations

# Capability identifiers (stable strings stored in Peer.metadata).
CAP_DELIVERY_RECEIPTS = "delivery_receipts"

CURRENT_HOOK_VERSION = 1
CURRENT_HOOK_CAPABILITIES: tuple[str, ...] = (CAP_DELIVERY_RECEIPTS,)


def current_capabilities_metadata() -> dict:
    """Return the metadata fields a current hook should advertise at registration."""
    return {
        "hook_version": CURRENT_HOOK_VERSION,
        "capabilities": list(CURRENT_HOOK_CAPABILITIES),
    }
