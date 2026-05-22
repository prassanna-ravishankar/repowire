"""Health check endpoint."""

from __future__ import annotations

import importlib.util
import json
import shutil
from typing import Any, Literal

from fastapi import APIRouter, Request
from pydantic import BaseModel, Field

from repowire import __version__

router = APIRouter(tags=["health"])


class ChannelHealth(BaseModel):
    """Passive Claude Code channel transport health."""

    status: Literal["inactive", "ready", "degraded"]
    configured: bool = False
    runtime_available: bool = False
    stop_hook_fallback: bool = True
    last_error: str | None = None


class AcpPermissionHealth(BaseModel):
    """Passive ACP permission relay health."""

    pending: int = 0
    timeout_seconds: float | None = None
    last_error: str | None = None
    last_error_at: str | None = None


class AcpBrokerHealth(BaseModel):
    """Passive broker-side ACP health."""

    status: Literal["inactive", "ready", "busy", "degraded"]
    enabled: bool = False
    sdk_available: bool = False
    manager_initialized: bool = False
    configured_peers: int = 0
    active_clients: int = 0
    in_flight: int = 0
    last_error: str | None = None
    last_error_peer_id: str | None = None
    last_error_at: str | None = None
    last_success_at: str | None = None
    permissions: AcpPermissionHealth = Field(default_factory=AcpPermissionHealth)


class HealthResponse(BaseModel):
    """Health check response."""

    status: str
    version: str
    relay_mode: bool = False
    channel: ChannelHealth
    acp_broker: AcpBrokerHealth


@router.get("/health", response_model=HealthResponse)
async def health_check(request: Request) -> HealthResponse:
    """Check daemon health status."""
    relay_mode = getattr(request.app.state, "relay_mode", False)
    config = getattr(request.app.state, "config", None)

    return HealthResponse(
        status="ok",
        version=__version__,
        relay_mode=relay_mode,
        channel=_channel_health(config),
        acp_broker=await _acp_broker_health(request, config),
    )


def _channel_health(config: Any) -> ChannelHealth:
    from repowire.installers.claude_code import CLAUDE_JSON

    entry = _channel_config_entry(CLAUDE_JSON)
    configured = entry is not None
    runtime_available = shutil.which("bun") is not None
    if not configured:
        return ChannelHealth(
            status="inactive",
            configured=False,
            runtime_available=runtime_available,
            stop_hook_fallback=True,
        )
    last_error = None
    expected_token = getattr(getattr(config, "daemon", None), "auth_token", None)
    configured_token = None
    env = entry.get("env") if isinstance(entry, dict) else None
    if isinstance(env, dict):
        configured_token = env.get("REPOWIRE_AUTH_TOKEN")
    if not runtime_available:
        last_error = "bun runtime not found"
    elif expected_token and configured_token != expected_token:
        last_error = "channel auth token differs from daemon auth token"
    elif not expected_token and configured_token:
        last_error = "channel auth token is set but daemon auth is disabled"
    return ChannelHealth(
        status="degraded" if last_error else "ready",
        configured=True,
        runtime_available=runtime_available,
        stop_hook_fallback=True,
        last_error=last_error,
    )


def _channel_config_entry(path: Any) -> dict[str, Any] | None:
    try:
        config = json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None
    entry = config.get("mcpServers", {}).get("repowire-channel")
    return entry if isinstance(entry, dict) else None


async def _acp_broker_health(request: Request, config: Any) -> AcpBrokerHealth:
    enabled = bool(getattr(getattr(config, "experiments", None), "acp_broker_client", False))
    sdk_available = importlib.util.find_spec("acp") is not None
    manager = getattr(request.app.state, "acp_manager", None)
    snapshot = _health_snapshot(manager)
    permission_snapshot = _health_snapshot(
        getattr(request.app.state, "acp_permission_broker", None),
    )
    configured_peers = await _count_acp_peers(getattr(request.app.state, "peer_registry", None))

    last_error = snapshot.get("last_error")
    permissions = AcpPermissionHealth(**permission_snapshot)
    if not enabled:
        status: Literal["inactive", "ready", "busy", "degraded"] = "inactive"
    elif not sdk_available:
        status = "degraded"
        last_error = last_error or "agent-client-protocol SDK not installed"
    elif manager is None:
        status = "degraded"
        last_error = last_error or "ACP client manager not initialized"
    elif snapshot.get("closed"):
        status = "degraded"
        last_error = last_error or "ACP client manager is closed"
    elif permissions.last_error:
        status = "degraded"
        last_error = last_error or permissions.last_error
    elif int(snapshot.get("in_flight") or 0) > 0:
        status = "busy"
    elif last_error:
        status = "degraded"
    else:
        status = "ready"

    return AcpBrokerHealth(
        status=status,
        enabled=enabled,
        sdk_available=sdk_available,
        manager_initialized=bool(snapshot.get("manager_initialized")),
        configured_peers=configured_peers,
        active_clients=int(snapshot.get("active_clients") or 0),
        in_flight=int(snapshot.get("in_flight") or 0),
        last_error=last_error,
        last_error_peer_id=snapshot.get("last_error_peer_id"),
        last_error_at=snapshot.get("last_error_at"),
        last_success_at=snapshot.get("last_success_at"),
        permissions=permissions,
    )


def _health_snapshot(obj: Any) -> dict[str, Any]:
    if obj is None or not hasattr(obj, "health_snapshot"):
        return {}
    snapshot = obj.health_snapshot()
    return snapshot if isinstance(snapshot, dict) else {}


async def _count_acp_peers(registry: Any) -> int:
    if registry is None or not hasattr(registry, "get_all_peers"):
        return 0
    peers = await registry.get_all_peers()
    return sum(1 for peer in peers if getattr(peer, "metadata", {}).get("acp"))
