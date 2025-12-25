from __future__ import annotations

import asyncio
import json
import signal
import socket
from typing import Any

import socketio

from repowire.config.models import Config, load_config
from repowire.protocol.messages import Message, MessageType
from repowire.session.manager import TmuxSessionManager


class RepowireDaemon:
    def __init__(self, config: Config | None = None) -> None:
        self.config = config or load_config()
        self.session_manager = TmuxSessionManager(self.config)
        self.machine = socket.gethostname()

        self._running = False
        self._sio: socketio.AsyncClient | None = None
        self._pending_relayed: dict[str, asyncio.Future[str]] = {}

    async def start(self) -> None:
        await self.session_manager.start()
        self._running = True

        if self.config.relay.enabled and self.config.relay.api_key:
            await self._connect_relay()

        await self._run_forever()

    async def stop(self) -> None:
        self._running = False

        if self._sio and self._sio.connected:
            await self._sio.disconnect()

        await self.session_manager.stop()

    async def _connect_relay(self) -> None:
        self._sio = socketio.AsyncClient(
            reconnection=True,
            reconnection_delay=1,
            reconnection_delay_max=30,
        )

        self._register_relay_handlers()

        try:
            await self._sio.connect(
                self.config.relay.url,
                auth={"api_key": self.config.relay.api_key},
                transports=["websocket"],
                wait_timeout=10,
            )

            for name, peer_config in self.config.peers.items():
                await self._sio.emit(
                    "register",
                    {
                        "name": name,
                        "path": peer_config.path,
                        "machine": self.machine,
                    },
                )

        except Exception as e:
            print(f"Failed to connect to relay: {e}")

    def _register_relay_handlers(self) -> None:
        if not self._sio:
            return

        @self._sio.on("connect")
        async def on_connect() -> None:
            print(f"Connected to relay: {self.config.relay.url}")

        @self._sio.on("disconnect")
        async def on_disconnect() -> None:
            print("Disconnected from relay")

        @self._sio.on("message")
        async def on_message(data: dict[str, Any]) -> None:
            await self._handle_relay_message(data)

        @self._sio.on("peer_joined")
        async def on_peer_joined(data: dict[str, Any]) -> None:
            print(f"Peer joined: {data.get('name')}")

        @self._sio.on("peer_left")
        async def on_peer_left(data: dict[str, Any]) -> None:
            print(f"Peer left: {data.get('name')}")

    async def _handle_relay_message(self, data: dict[str, Any]) -> None:
        msg = Message.from_dict(data)

        if not msg.to_peer or msg.to_peer not in self.config.peers:
            return

        from_peer = msg.from_peer
        to_peer = msg.to_peer
        text = msg.payload.get("text", "")
        correlation_id = msg.correlation_id

        if msg.type == MessageType.QUERY and correlation_id:
            try:
                response = await self.session_manager.send_query(to_peer, text, from_peer=from_peer)

                if self._sio:
                    await self._sio.emit(
                        "response",
                        {
                            "correlation_id": correlation_id,
                            "to_peer": from_peer,
                            "payload": {"text": response, "success": True},
                        },
                    )
            except Exception as e:
                if self._sio:
                    await self._sio.emit(
                        "response",
                        {
                            "correlation_id": correlation_id,
                            "to_peer": from_peer,
                            "payload": {"text": str(e), "success": False},
                        },
                    )

        elif msg.type == MessageType.NOTIFICATION:
            try:
                await self.session_manager.send_notification(to_peer, text, from_peer=from_peer)
            except Exception:
                pass

    async def _run_forever(self) -> None:
        stop_event = asyncio.Event()

        def handle_signal() -> None:
            stop_event.set()

        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, handle_signal)

        print(f"Repowire daemon started (peers: {list(self.config.peers.keys())})")

        await stop_event.wait()
        await self.stop()


async def run_daemon(config: Config | None = None) -> None:
    daemon = RepowireDaemon(config)
    await daemon.start()
