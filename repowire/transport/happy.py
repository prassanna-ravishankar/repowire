"""Happy Cloud WebSocket transport.

This module implements the Happy transport adapter for peer-to-peer communication.
It connects to Happy Cloud via Socket.io WebSocket and sends encrypted messages.

Reference files:
- happy/sources/sync/apiSocket.ts - WebSocket protocol
- happy/sources/sync/sync.ts - Message sending logic
"""

from __future__ import annotations

import asyncio
import base64
import uuid
from typing import Any

import httpx
import socketio

from repowire.auth.happy import (
    DEFAULT_HAPPY_URL,
    HappyCredentials,
    decode_secret,
    load_credentials,
)
from repowire.transport.base import Transport
from repowire.transport.happy_encryption import HappyEncryption, SessionEncryption


class HappyTransport(Transport):
    """Transport for Happy Cloud sessions.

    This transport connects to Happy Cloud via Socket.io WebSocket and
    sends messages to Happy CLI sessions.
    """

    def __init__(
        self,
        credentials: HappyCredentials | None = None,
        server_url: str = DEFAULT_HAPPY_URL,
    ) -> None:
        self.credentials = credentials or load_credentials()
        if not self.credentials:
            raise ValueError(
                "Happy credentials not found. Run 'repowire auth happy' first."
            )

        self.server_url = server_url
        self.sio = socketio.AsyncClient(reconnection=True, reconnection_delay=1)
        self._connected = False
        self._connecting = False

        # Initialize encryption from master secret
        master_secret = decode_secret(self.credentials.secret)
        self.encryption = HappyEncryption(master_secret)

        # Session data cache
        self._sessions: dict[str, dict[str, Any]] = {}
        self._pending_responses: dict[str, asyncio.Future[str]] = {}

    async def connect(self) -> None:
        """Connect to Happy Cloud WebSocket."""
        if self._connected or self._connecting:
            return

        self._connecting = True

        @self.sio.on("connect")
        async def on_connect() -> None:
            self._connected = True
            self._connecting = False

        @self.sio.on("disconnect")
        async def on_disconnect() -> None:
            self._connected = False

        @self.sio.on("update")
        async def on_update(data: dict[str, Any]) -> None:
            await self._handle_update(data)

        try:
            await self.sio.connect(
                self.server_url,
                socketio_path="/v1/updates",
                auth={
                    "token": self.credentials.token,
                    "clientType": "user-scoped",
                },
                transports=["websocket"],
            )
        except Exception as e:
            self._connecting = False
            raise ConnectionError(f"Failed to connect to Happy Cloud: {e}")

    async def _ensure_connected(self) -> None:
        """Ensure we're connected to Happy Cloud."""
        if not self._connected:
            await self.connect()

    async def _fetch_sessions(self) -> None:
        """Fetch all sessions from Happy Cloud."""
        async with httpx.AsyncClient() as client:
            response = await client.get(
                f"{self.server_url}/v1/sessions",
                headers={"Authorization": f"Bearer {self.credentials.token}"},
            )
            response.raise_for_status()
            data = response.json()

            for session in data.get("sessions", []):
                session_id = session["id"]
                self._sessions[session_id] = session

                # Initialize encryption for this session
                data_key = None
                if session.get("dataEncryptionKey"):
                    data_key = self.encryption.decrypt_encryption_key(
                        session["dataEncryptionKey"]
                    )
                self.encryption.initialize_session(session_id, data_key)

    async def _get_session_encryption(
        self, session_id: str
    ) -> SessionEncryption | None:
        """Get encryption for a session, fetching sessions if needed."""
        session_enc = self.encryption.get_session_encryption(session_id)
        if session_enc is None:
            await self._fetch_sessions()
            session_enc = self.encryption.get_session_encryption(session_id)
        return session_enc

    async def _handle_update(self, data: dict[str, Any]) -> None:
        """Handle incoming update from Happy Cloud."""
        update_type = data.get("body", {}).get("t")

        if update_type == "new-message":
            await self._handle_new_message(data)

    async def _handle_new_message(self, data: dict[str, Any]) -> None:
        """Handle a new message update."""
        body = data.get("body", {})
        session_id = body.get("sid")
        message = body.get("message", {})

        if not session_id or not message:
            return

        # Check if this is a response to a pending query
        message_id = message.get("id")
        local_id = message.get("localId")

        # Try to match to a pending response
        for query_id, future in list(self._pending_responses.items()):
            if not future.done():
                # Decrypt the message
                session_enc = await self._get_session_encryption(session_id)
                if session_enc and message.get("content", {}).get("t") == "encrypted":
                    content = session_enc.decrypt_raw(message["content"]["c"])
                    if content and content.get("role") == "assistant":
                        # Extract text from assistant response
                        msg_content = content.get("content", {})
                        if isinstance(msg_content, dict):
                            text = msg_content.get("text", "")
                        elif isinstance(msg_content, list):
                            text = " ".join(
                                c.get("text", "")
                                for c in msg_content
                                if c.get("type") == "text"
                            )
                        else:
                            text = str(msg_content)

                        future.set_result(text)
                        del self._pending_responses[query_id]
                        break

    async def send_message(self, session_id: str, text: str) -> str:
        """Send message to Happy session and wait for response."""
        await self._ensure_connected()

        session_enc = await self._get_session_encryption(session_id)
        if not session_enc:
            raise ValueError(f"Session {session_id} not found or not initialized")

        # Create RawRecord
        local_id = str(uuid.uuid4())
        raw_record = {
            "role": "user",
            "content": {"type": "text", "text": text},
            "meta": {
                "sentFrom": "repowire",
                "permissionMode": "default",
            },
        }

        # Encrypt the message
        encrypted_message = session_enc.encrypt_raw_record(raw_record)

        # Create a future for the response
        query_id = str(uuid.uuid4())
        response_future: asyncio.Future[str] = asyncio.Future()
        self._pending_responses[query_id] = response_future

        # Send via socket
        self.sio.emit(
            "message",
            {
                "sid": session_id,
                "message": encrypted_message,
                "localId": local_id,
                "sentFrom": "repowire",
                "permissionMode": "default",
            },
        )

        # Wait for response with timeout
        try:
            response = await asyncio.wait_for(response_future, timeout=120.0)
            return response
        except asyncio.TimeoutError:
            if query_id in self._pending_responses:
                del self._pending_responses[query_id]
            raise
        except Exception:
            if query_id in self._pending_responses:
                del self._pending_responses[query_id]
            raise

    async def send_notification(self, session_id: str, text: str) -> None:
        """Send fire-and-forget notification to Happy session."""
        await self._ensure_connected()

        session_enc = await self._get_session_encryption(session_id)
        if not session_enc:
            raise ValueError(f"Session {session_id} not found or not initialized")

        local_id = str(uuid.uuid4())
        raw_record = {
            "role": "user",
            "content": {"type": "text", "text": text},
            "meta": {
                "sentFrom": "repowire",
                "permissionMode": "default",
            },
        }

        encrypted_message = session_enc.encrypt_raw_record(raw_record)

        self.sio.emit(
            "message",
            {
                "sid": session_id,
                "message": encrypted_message,
                "localId": local_id,
                "sentFrom": "repowire",
                "permissionMode": "default",
            },
        )

    async def close(self) -> None:
        """Disconnect from Happy Cloud."""
        if self._connected:
            await self.sio.disconnect()
            self._connected = False
