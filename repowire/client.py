"""Programmatic client for the Repowire daemon protocol.

This module is intentionally a thin wrapper around the public daemon HTTP API.
It gives agents and apps a stable typed surface without reaching into daemon
internals or duplicating route-specific request code.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any, Literal
from urllib.parse import quote

import httpx
from pydantic import BaseModel, Field

from repowire.config.models import DEFAULT_DAEMON_URL
from repowire.protocol.errors import DaemonConnectionError, DaemonHTTPError, DaemonTimeoutError
from repowire.protocol.messages import AttachmentRef
from repowire.protocol.peers import PeerRole


class ChannelHealth(BaseModel):
    """Passive Claude Code channel transport health."""

    status: str = "inactive"
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

    status: str = "inactive"
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


class Health(BaseModel):
    """Daemon health response."""

    status: str
    version: str
    relay_mode: bool = False
    channel: ChannelHealth = Field(default_factory=ChannelHealth)
    acp_broker: AcpBrokerHealth = Field(default_factory=AcpBrokerHealth)


class ClientPeer(BaseModel):
    """Peer as returned by the daemon HTTP API."""

    peer_id: str
    name: str
    display_name: str
    path: str | None = None
    machine: str | None = None
    tmux_session: str | None = None
    backend: str = "claude-code"
    model: str | None = None
    circle: str = "default"
    role: PeerRole = PeerRole.AGENT
    status: str
    last_seen: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    description: str = ""


class RegisterPeerResult(BaseModel):
    """Result of registering a peer."""

    ok: bool = True
    peer_id: str
    display_name: str


class QueryResult(BaseModel):
    """Legacy blocking query result."""

    text: str | None = None
    error: str | None = None
    status: str | None = None


class AskResult(BaseModel):
    """Result of opening an ask thread."""

    correlation_id: str
    error: str | None = None


class PendingAsk(BaseModel):
    """Open ask involving a peer."""

    correlation_id: str
    from_peer: str
    to_peer: str
    text: str
    created_at: str
    direction: Literal["inbound", "outbound"]


class BroadcastResult(BaseModel):
    """Best-effort broadcast result."""

    ok: bool = True
    sent_to: list[str]
    failed: list[dict[str, str]] = Field(default_factory=list)


class JobResult(BaseModel):
    """Durable tracked-job create/run result."""

    job_id: str | None = None
    work_id: str | None = None
    status: dict[str, Any] = Field(default_factory=dict)


class MeshEvent(BaseModel):
    """Event emitted by the daemon event log."""

    id: str
    type: str
    timestamp: str

    model_config = {"extra": "allow"}


class ToolCall(BaseModel):
    """Tool call summary for dashboard chat events."""

    name: str
    input: str = ""


class SpawnConfigInfo(BaseModel):
    """Daemon spawn capability discovery."""

    enabled: bool
    commands: dict[str, str] = Field(default_factory=dict)
    profiles: dict[str, dict[str, dict[str, Any]]] = Field(default_factory=dict)
    allowed_commands: list[str] = Field(default_factory=list)
    allowed_paths: list[str] = Field(default_factory=list)


class SpawnResult(BaseModel):
    """Result of spawning a peer."""

    ok: bool = True
    display_name: str
    tmux_session: str
    peer_id: str | None = None
    registration_state: str = "pending_hook"
    warnings: list[str] = Field(default_factory=list)


class KillPeerResult(BaseModel):
    """Result of killing a registered peer."""

    ok: bool = True
    tmux_killed: bool | None = None


class RestartPeerResult(BaseModel):
    """Result of restarting a registered peer."""

    ok: bool = True
    status: str
    restarted: bool = False
    peer_id: str
    display_name: str
    backend: str
    path: str
    circle: str
    tmux_session: str | None = None
    resume_mode: str = "resumed"
    unsupported_reason: str | None = None
    command: str | None = None


class SessionResumeResult(BaseModel):
    """Result of inspecting or spawning backend-native session resume."""

    ok: bool = True
    repowire_session_id: str
    session_status: str
    status: str
    capability: str
    message: str
    backend: str
    runtime_session_id: str | None = None
    runtime_source_uri: str | None = None
    executor_peer_id: str | None = None
    executor_peer_name: str | None = None
    resume_capability: dict[str, Any] = Field(default_factory=dict)
    action: str = "inspect"
    spawned_display_name: str | None = None
    tmux_session: str | None = None
    pane_id: str | None = None


class AttachmentInfo(BaseModel):
    """Uploaded attachment metadata."""

    id: str
    path: str
    filename: str
    size: int
    content_type: str | None = None


class OrchestratorStatus(BaseModel):
    """Circle orchestrator liveness status."""

    circle: str
    present: bool
    peer_id: str | None = None
    peer_name: str | None = None
    last_seen: str | None = None
    stale_after_seconds: int


class AsyncRepowireClient:
    """Typed async client for the Repowire daemon HTTP API."""

    def __init__(
        self,
        base_url: str = DEFAULT_DAEMON_URL,
        *,
        auth_token: str | None = None,
        timeout: float | httpx.Timeout = 30.0,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self._owned_client = client is None
        self._auth_headers = {"Authorization": f"Bearer {auth_token}"} if auth_token else None
        self._client = client or httpx.AsyncClient(
            base_url=self.base_url,
            timeout=timeout,
        )

    async def __aenter__(self) -> AsyncRepowireClient:
        return self

    async def __aexit__(self, *exc_info: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        """Close the underlying HTTP client if this object owns it."""
        if self._owned_client:
            await self._client.aclose()

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
        files: dict[str, Any] | None = None,
    ) -> Any:
        try:
            resp = await self._client.request(
                method,
                path,
                json=json,
                params=params,
                files=files,
                headers=self._auth_headers,
            )
            resp.raise_for_status()
        except httpx.ConnectError as e:
            raise DaemonConnectionError() from e
        except httpx.TimeoutException as e:
            raise DaemonTimeoutError() from e
        except httpx.HTTPStatusError as e:
            raise DaemonHTTPError(e.response.status_code, e.response.text) from e

        if not resp.content:
            return None
        content_type = resp.headers.get("content-type", "")
        if content_type.startswith("application/json"):
            return resp.json()
        return resp.content

    async def health(self) -> Health:
        """Fetch daemon health."""
        return Health.model_validate(await self._request("GET", "/health"))

    async def list_peers(
        self,
        *,
        status: str | None = None,
        path: str | None = None,
        backend: str | None = None,
    ) -> list[ClientPeer]:
        """List registered peers, optionally filtered by daemon-supported params."""
        params = {
            k: v
            for k, v in {"status": status, "path": path, "backend": backend}.items()
            if v is not None
        }
        data = await self._request("GET", "/peers", params=params or None)
        return [ClientPeer.model_validate(p) for p in data.get("peers", [])]

    async def get_peer(self, identifier: str, *, circle: str | None = None) -> ClientPeer:
        """Get a peer by peer_id or display_name."""
        params = {"circle": circle} if circle is not None else None
        data = await self._request("GET", f"/peers/{quote(identifier, safe='')}", params=params)
        return ClientPeer.model_validate(data)

    async def get_peer_by_pane(self, pane_id: str) -> ClientPeer:
        """Get a peer by tmux pane id."""
        data = await self._request("GET", f"/peers/by-pane/{quote(pane_id, safe='')}")
        return ClientPeer.model_validate(data)

    async def register_peer(
        self,
        name: str,
        *,
        path: str | None = None,
        machine: str | None = None,
        tmux_session: str | None = None,
        pane_id: str | None = None,
        backend: str = "claude-code",
        circle: str | None = None,
        circle_source: str | None = None,
        role: str = "agent",
        metadata: dict[str, Any] | None = None,
    ) -> RegisterPeerResult:
        """Register a peer and return daemon-assigned identity."""
        payload: dict[str, Any] = {
            "name": name,
            "backend": backend,
            "role": role,
            "metadata": metadata or {},
        }
        for key, value in {
            "path": path,
            "machine": machine,
            "tmux_session": tmux_session,
            "pane_id": pane_id,
            "circle": circle,
            "circle_source": circle_source,
        }.items():
            if value is not None:
                payload[key] = value
        return RegisterPeerResult.model_validate(
            await self._request("POST", "/peers", json=payload)
        )

    async def unregister_peer(self, name: str, *, circle: str | None = None) -> None:
        """Unregister a peer by peer_id or display_name."""
        params = {"circle": circle} if circle is not None else None
        await self._request("DELETE", f"/peers/{quote(name, safe='')}", params=params)

    async def mark_peer_offline(self, name: str) -> int:
        """Mark a peer offline and return the number of cancelled queries."""
        data = await self._request("POST", f"/peers/{quote(name, safe='')}/offline")
        return int(data.get("cancelled_queries", 0))

    async def set_description(
        self,
        name: str,
        description: str,
        *,
        circle: str | None = None,
    ) -> None:
        """Update a peer's task description."""
        params = {"circle": circle} if circle is not None else None
        await self._request(
            "POST",
            f"/peers/{quote(name, safe='')}/description",
            json={"description": description},
            params=params,
        )

    async def touch_peer(self, name: str, *, circle: str | None = None) -> None:
        """Refresh a peer's last_seen without changing status."""
        params = {"circle": circle} if circle is not None else None
        await self._request("POST", f"/peers/{quote(name, safe='')}/touch", params=params)

    async def set_circle(self, peer_name: str, circle: str) -> None:
        """Set a peer's circle."""
        await self._request(
            "POST",
            "/peers/circle",
            json={"peer_name": peer_name, "circle": circle},
        )

    async def orchestrator_status(self, circle: str) -> OrchestratorStatus:
        """Return liveness status for a circle's orchestrator."""
        data = await self._request("GET", f"/circles/{quote(circle, safe='')}/orchestrator")
        return OrchestratorStatus.model_validate(data)

    async def query(
        self,
        to_peer: str,
        text: str,
        *,
        from_peer: str | None = None,
        timeout: float | None = None,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> QueryResult:
        """Open a blocking compatibility ask and wait for a response.

        The daemon keeps ``/query`` as the old blocking RPC surface, but it is
        backed by the ask/answer lifecycle.
        """
        payload: dict[str, Any] = {
            "to_peer": to_peer,
            "text": text,
            "bypass_circle": bypass_circle,
        }
        for key, value in {
            "from_peer": from_peer,
            "timeout": timeout,
            "circle": circle,
        }.items():
            if value is not None:
                payload[key] = value
        return QueryResult.model_validate(await self._request("POST", "/query", json=payload))

    async def ask(
        self,
        to_peer: str,
        text: str,
        *,
        from_peer: str,
        attachments: list[AttachmentRef | dict[str, Any]] | None = None,
        reply_to: str | None = None,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> AskResult:
        """Open a non-blocking ask thread."""
        payload: dict[str, Any] = {
            "from_peer": from_peer,
            "to_peer": to_peer,
            "text": text,
            "bypass_circle": bypass_circle,
        }
        if reply_to is not None:
            payload["reply_to"] = reply_to
        if attachments:
            payload["attachments"] = [
                a.model_dump(exclude_none=True) if isinstance(a, AttachmentRef) else a
                for a in attachments
            ]
        if circle is not None:
            payload["circle"] = circle
        return AskResult.model_validate(await self._request("POST", "/ask", json=payload))

    async def ack(
        self,
        correlation_id: str,
        *,
        from_peer: str,
        message: str | None = None,
        attachments: list[AttachmentRef | dict[str, Any]] | None = None,
    ) -> None:
        """Close an ask, optionally delivering a reply message."""
        payload: dict[str, Any] = {
            "correlation_id": correlation_id,
            "from_peer": from_peer,
        }
        if message is not None:
            payload["message"] = message
        if attachments:
            payload["attachments"] = [
                a.model_dump(exclude_none=True) if isinstance(a, AttachmentRef) else a
                for a in attachments
            ]
        await self._request("POST", "/ack", json=payload)

    async def pending_asks(
        self,
        *,
        pane_id: str | None = None,
        peer_id: str | None = None,
        direction: Literal["inbound", "outbound", "both"] = "inbound",
    ) -> list[PendingAsk]:
        """List open asks for exactly one pane_id or peer_id."""
        params = {
            k: v
            for k, v in {
                "pane_id": pane_id,
                "peer_id": peer_id,
                "direction": direction,
            }.items()
            if v
        }
        data = await self._request("GET", "/asks/pending", params=params)
        return [PendingAsk.model_validate(a) for a in data.get("asks", [])]

    async def notify(
        self,
        to_peer: str,
        text: str,
        *,
        from_peer: str,
        attachments: list[AttachmentRef | dict[str, Any]] | None = None,
        bypass_circle: bool = False,
        circle: str | None = None,
    ) -> None:
        """Send a fire-and-forget notification."""
        payload: dict[str, Any] = {
            "from_peer": from_peer,
            "to_peer": to_peer,
            "text": text,
            "bypass_circle": bypass_circle,
        }
        if circle is not None:
            payload["circle"] = circle
        if attachments:
            payload["attachments"] = [
                a.model_dump(exclude_none=True) if isinstance(a, AttachmentRef) else a
                for a in attachments
            ]
        await self._request("POST", "/notify", json=payload)

    async def broadcast(
        self,
        text: str,
        *,
        from_peer: str,
        exclude: list[str] | None = None,
        bypass_circle: bool = False,
    ) -> BroadcastResult:
        """Broadcast a message to eligible peers."""
        data = await self._request(
            "POST",
            "/broadcast",
            json={
                "from_peer": from_peer,
                "text": text,
                "exclude": exclude or [],
                "bypass_circle": bypass_circle,
            },
        )
        return BroadcastResult.model_validate(data)

    async def create_job(
        self,
        title: str = "",
        *,
        kind: str = "general",
        prompt: str | None = None,
        path: str | None = None,
        backend: str | None = None,
        circle: str | None = None,
        owner_peer_id: str | None = None,
        source_kind: str | None = None,
        source_id: str | None = None,
        provenance: dict[str, Any] | None = None,
        due_at: str | None = None,
        auto_run: bool = False,
    ) -> JobResult:
        """Create a durable tracked job, optionally firing it immediately.

        Wraps ``POST /jobs``; ``auto_run`` follows with ``/jobs/{id}/run`` so a
        one-shot trigger (e.g. a webhook) both records the job and dispatches it.
        """
        payload: dict[str, Any] = {"title": title, "kind": kind}
        for key, value in {
            "prompt": prompt,
            "path": path,
            "backend": backend,
            "circle": circle,
            "owner_peer_id": owner_peer_id,
            "source_kind": source_kind,
            "source_id": source_id,
            "due_at": due_at,
        }.items():
            if value is not None:
                payload[key] = value
        if provenance:
            payload["provenance"] = provenance
        result = JobResult.model_validate(await self._request("POST", "/jobs", json=payload))
        if auto_run and result.work_id:
            await self.run_job(result.work_id)
        return result

    async def run_job(
        self, work_id: str, *, requested_by_peer_id: str | None = None
    ) -> JobResult:
        """Fire a tracked job on demand (the manual trigger source for jobs)."""
        payload: dict[str, Any] = {}
        if requested_by_peer_id is not None:
            payload["requested_by_peer_id"] = requested_by_peer_id
        data = await self._request("POST", f"/jobs/{quote(work_id, safe='')}/run", json=payload)
        return JobResult.model_validate(data)

    async def events(self) -> list[MeshEvent]:
        """Return recent daemon events."""
        data = await self._request("GET", "/events")
        return [MeshEvent.model_validate(e) for e in data]

    async def ingest_chat_turn(
        self,
        peer: str,
        role: str,
        text: str,
        *,
        peer_id: str | None = None,
        pane_id: str | None = None,
        tool_calls: list[ToolCall] | None = None,
    ) -> None:
        """Ingest a chat turn for dashboard display."""
        payload: dict[str, Any] = {"peer": peer, "role": role, "text": text}
        if peer_id is not None:
            payload["peer_id"] = peer_id
        if pane_id is not None:
            payload["pane_id"] = pane_id
        if tool_calls is not None:
            payload["tool_calls"] = [tc.model_dump() for tc in tool_calls]
        await self._request("POST", "/events/chat", json=payload)

    async def spawn_config(self) -> SpawnConfigInfo:
        """Return daemon spawn configuration."""
        return SpawnConfigInfo.model_validate(await self._request("GET", "/spawn/config"))

    async def spawn(
        self,
        path: str,
        command: str | None = None,
        *,
        backend: str | None = None,
        profile: str | None = None,
        circle: str | None = None,
        message: str | None = None,
    ) -> SpawnResult:
        """Spawn a new agent session via the daemon."""
        if backend is not None and command is not None:
            raise ValueError("Pass backend or command, not both")
        if backend is None and command is None:
            raise ValueError("Pass backend or command")
        if profile is not None and backend is None:
            raise ValueError("Pass backend with profile")
        payload = {"path": path}
        if backend is not None:
            payload["backend"] = backend
        if profile is not None:
            payload["profile"] = profile
        if command is not None:
            payload["command"] = command
        if circle is not None:
            payload["circle"] = circle
        if message is not None:
            payload["message"] = message
        return SpawnResult.model_validate(await self._request("POST", "/spawn", json=payload))

    async def kill_peer(
        self,
        peer_identifier: str,
        *,
        circle: str | None = None,
        from_peer: str | None = None,
    ) -> KillPeerResult:
        """Kill a registered peer, and possibly its daemon-owned tmux pane."""
        payload: dict[str, Any] = {"peer_identifier": peer_identifier}
        if circle is not None:
            payload["circle"] = circle
        if from_peer is not None:
            payload["from_peer"] = from_peer
        return KillPeerResult.model_validate(
            await self._request("POST", "/kill-peer", json=payload)
        )

    async def restart_peer(
        self,
        peer_identifier: str,
        *,
        circle: str | None = None,
        from_peer: str | None = None,
        dry_run: bool = False,
        message: str | None = None,
    ) -> RestartPeerResult:
        """Restart a registered daemon-spawned peer on the same backend."""
        payload: dict[str, Any] = {"dry_run": dry_run}
        if circle is not None:
            payload["circle"] = circle
        if from_peer is not None:
            payload["from_peer"] = from_peer
        if message is not None:
            payload["message"] = message
        return RestartPeerResult.model_validate(
            await self._request(
                "POST",
                f"/peers/{quote(peer_identifier, safe='')}/restart",
                json=payload,
            )
        )

    async def resume_session(
        self,
        repowire_session_id: str,
        *,
        dry_run: bool = True,
        profile: str | None = None,
        message: str | None = None,
        from_peer: str | None = None,
    ) -> SessionResumeResult:
        """Inspect or spawn backend-native resume for a durable Repowire session."""
        payload: dict[str, Any] = {"dry_run": dry_run}
        if profile is not None:
            payload["profile"] = profile
        if message is not None:
            payload["message"] = message
        if from_peer is not None:
            payload["from_peer"] = from_peer
        return SessionResumeResult.model_validate(
            await self._request(
                "POST",
                f"/sessions/{quote(repowire_session_id, safe='')}/controls/resume",
                json=payload,
            )
        )

    async def upload_attachment(
        self,
        file: bytes | str | Path,
        *,
        filename: str | None = None,
        content_type: str = "application/octet-stream",
    ) -> AttachmentInfo:
        """Upload an attachment and return its daemon-local metadata."""
        close_after = None
        if isinstance(file, bytes):
            upload_name = filename or "file.bin"
            file_value: Any = (upload_name, file, content_type)
        else:
            path = Path(file)
            upload_name = filename or path.name
            close_after = path.open("rb")
            file_value = (upload_name, close_after, content_type)

        try:
            data = await self._request("POST", "/attachments", files={"file": file_value})
        finally:
            if close_after is not None:
                close_after.close()
        return AttachmentInfo.model_validate(data)

    async def download_attachment(self, attachment_id: str) -> bytes:
        """Download an attachment by ID."""
        data = await self._request("GET", f"/attachments/{quote(attachment_id, safe='')}")
        if isinstance(data, bytes):
            return data
        return bytes(data or b"")


__all__ = [
    "AskResult",
    "AsyncRepowireClient",
    "AttachmentInfo",
    "BroadcastResult",
    "ClientPeer",
    "Health",
    "KillPeerResult",
    "MeshEvent",
    "OrchestratorStatus",
    "PendingAsk",
    "QueryResult",
    "RegisterPeerResult",
    "RestartPeerResult",
    "SessionResumeResult",
    "SpawnConfigInfo",
    "SpawnResult",
    "ToolCall",
]
