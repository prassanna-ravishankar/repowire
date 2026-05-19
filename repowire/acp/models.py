"""Pydantic shapes the broker uses to describe ACP-routed peers.

These are *broker-side* models — the on-wire ACP messages themselves come from
the `agent-client-protocol` SDK (`acp.PromptRequest`, `acp.SessionNotification`,
etc.). This module only models the configuration the broker needs to spawn and
talk to an ACP agent subprocess on behalf of a registered peer.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class AcpPeerConfig(BaseModel):
    """How the broker spawns and talks to an ACP-routed peer.

    Persisted in `Peer.metadata["acp"]`. When the experiments flag is on and a
    peer carries this block, the broker bypasses the WS transport and routes
    asks through an ACP subprocess instead.
    """

    model_config = ConfigDict(extra="ignore")

    command: str = Field(..., description="Executable that speaks ACP over stdio")
    args: list[str] = Field(default_factory=list, description="Args appended to command")
    cwd: str | None = Field(
        None,
        description=(
            "Working directory for the subprocess; defaults to the peer's "
            "registered `path` if absent."
        ),
    )
    env: dict[str, str] | None = Field(
        None,
        description="Extra environment overlaid on the daemon's environment.",
    )
