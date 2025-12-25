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
from repowire.transport.happy_encryption import HappyEncryption, SessionEncryption


class HappyTransport:
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
        self._connected = False
        self._connecting = False
        self._handlers_registered = False

        # Initialize encryption from master secret
        master_secret = decode_secret(self.credentials.secret)
        self.encryption = HappyEncryption(master_secret)

        # Session data cache
        self._sessions: dict[str, dict[str, Any]] = {}
        self._pending_responses: dict[str, asyncio.Future[str]] = {}

        # Create socket client
        self.sio = socketio.AsyncClient(
            reconnection=True,
            reconnection_delay=1,
            logger=False,
            engineio_logger=False,
        )
        self._register_handlers()

    def _register_handlers(self) -> None:
        """Register socket event handlers once."""
        if self._handlers_registered:
            return

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

        self._handlers_registered = True

    async def connect(self) -> None:
        """Connect to Happy Cloud WebSocket."""
        if self._connected:
            return

        if self._connecting:
            # Wait for existing connection attempt
            for _ in range(50):  # 5 seconds max
                await asyncio.sleep(0.1)
                if self._connected:
                    return
            raise ConnectionError("Connection timeout")

        self._connecting = True

        try:
            await self.sio.connect(
                self.server_url,
                socketio_path="/v1/updates",
                auth={
                    "token": self.credentials.token,
                    "clientType": "user-scoped",
                },
                transports=["websocket"],
                wait_timeout=10,
            )
            # Wait briefly for connect event
            for _ in range(20):
                await asyncio.sleep(0.1)
                if self._connected:
                    return
        except Exception as e:
            self._connecting = False
            raise ConnectionError(f"Failed to connect to Happy Cloud: {e}")
        finally:
            self._connecting = False

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

                # Initialize encryption for this session first
                data_key = None
                if session.get("dataEncryptionKey"):
                    data_key = self.encryption.decrypt_encryption_key(
                        session["dataEncryptionKey"]
                    )
                session_enc = self.encryption.initialize_session(session_id, data_key)

                # Decrypt metadata using session-specific encryption
                metadata = session.get("metadata")
                if isinstance(metadata, str) and metadata:
                    decrypted = session_enc.decrypt_raw(metadata)
                    if decrypted and isinstance(decrypted, dict):
                        session["metadata"] = decrypted
                    else:
                        session["metadata"] = {}

                self._sessions[session_id] = session

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
        import logging
        logger = logging.getLogger(__name__)

        update_type = data.get("body", {}).get("t")
        session_id = data.get("body", {}).get("sid", "unknown")
        logger.info(f"[HAPPY] Received update type={update_type} session={session_id}")

        if update_type == "new-message":
            await self._handle_new_message(data)

    async def _handle_new_message(self, data: dict[str, Any]) -> None:
        """Handle a new message update."""
        import logging
        logger = logging.getLogger(__name__)

        body = data.get("body", {})
        session_id = body.get("sid")
        message = body.get("message", {})

        logger.info(f"[HAPPY] Received new-message for session {session_id}")

        if not session_id or not message:
            return

        # Try to match to a pending response
        for query_id, future in list(self._pending_responses.items()):
            if not future.done():
                # Decrypt the message
                session_enc = await self._get_session_encryption(session_id)
                if session_enc and message.get("content", {}).get("t") == "encrypted":
                    content = session_enc.decrypt_raw(message["content"]["c"])
                    logger.info(f"[HAPPY] Decrypted message role={content.get('role') if content else 'None'}")

                    # CLI sends role="agent" for assistant messages, not "assistant"
                    if content and content.get("role") in ("assistant", "agent"):
                        # Extract text from the response
                        # CLI wraps content in {type: "output", data: {...}}
                        msg_content = content.get("content", {})
                        text = ""

                        if isinstance(msg_content, dict):
                            if msg_content.get("type") == "output":
                                # CLI format: {type: "output", data: {type: "assistant", message: {...}}}
                                output_data = msg_content.get("data", {})
                                if isinstance(output_data, dict):
                                    inner_msg = output_data.get("message", {})
                                    if isinstance(inner_msg, dict):
                                        inner_content = inner_msg.get("content", [])
                                        if isinstance(inner_content, list):
                                            text = " ".join(
                                                c.get("text", "")
                                                for c in inner_content
                                                if isinstance(c, dict) and c.get("type") == "text"
                                            )
                                        elif isinstance(inner_content, str):
                                            text = inner_content
                            else:
                                text = msg_content.get("text", "")
                        elif isinstance(msg_content, list):
                            text = " ".join(
                                c.get("text", "")
                                for c in msg_content
                                if isinstance(c, dict) and c.get("type") == "text"
                            )
                        else:
                            text = str(msg_content)

                        if text:
                            logger.info(f"[HAPPY] Got response text: {text[:100]}...")
                            future.set_result(text)
                            del self._pending_responses[query_id]
                            break

    async def send_message(self, session_id: str, text: str, permission_mode: str = "default") -> str:
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
                "permissionMode": permission_mode,
            },
        }

        # Encrypt the message
        encrypted_message = session_enc.encrypt_raw_record(raw_record)

        # Create a future for the response
        query_id = str(uuid.uuid4())
        response_future: asyncio.Future[str] = asyncio.Future()
        self._pending_responses[query_id] = response_future

        # Send via socket
        await self.sio.emit(
            "message",
            {
                "sid": session_id,
                "message": encrypted_message,
                "localId": local_id,
                "sentFrom": "repowire",
                "permissionMode": permission_mode,
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

    async def send_notification(self, session_id: str, text: str, permission_mode: str = "default") -> None:
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
                "permissionMode": permission_mode,
            },
        }

        encrypted_message = session_enc.encrypt_raw_record(raw_record)

        await self.sio.emit(
            "message",
            {
                "sid": session_id,
                "message": encrypted_message,
                "localId": local_id,
                "sentFrom": "repowire",
                "permissionMode": permission_mode,
            },
        )

    async def list_sessions(self) -> list[dict[str, Any]]:
        """List all available sessions.

        Returns:
            List of session dicts with: id, path, host, active, metadata
        """
        await self._fetch_sessions()

        sessions = []
        for session_id, session in self._sessions.items():
            metadata = session.get("metadata") or {}
            # Handle case where metadata might be a string or None
            if not isinstance(metadata, dict):
                metadata = {}
            sessions.append({
                "id": session_id,
                "path": metadata.get("path"),
                "host": metadata.get("host"),
                "active": session.get("active", False),
                "metadata": metadata,
            })

        return sessions

    async def close(self) -> None:
        """Disconnect from Happy Cloud."""
        if self._connected:
            await self.sio.disconnect()
            self._connected = False
