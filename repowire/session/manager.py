"""Tmux session manager for Repowire."""

from __future__ import annotations

import asyncio
import json
import socket
from datetime import datetime
from pathlib import Path
from uuid import uuid4

import libtmux

from repowire.config.models import Config, load_config
from repowire.protocol.peers import Peer, PeerStatus


class TmuxSessionManager:
    def __init__(self, config: Config | None = None) -> None:
        self.config = config or load_config()
        self.server = libtmux.Server()
        self.pending_dir = Path.home() / ".repowire" / "pending"
        self.socket_path = Path(self.config.daemon.socket_path)

        self._pending_futures: dict[str, asyncio.Future[str]] = {}
        self._socket_server: asyncio.Server | None = None
        self._running = False

    async def start(self) -> None:
        self.pending_dir.mkdir(parents=True, exist_ok=True)
        self.socket_path.parent.mkdir(parents=True, exist_ok=True)

        if self.socket_path.exists():
            self.socket_path.unlink()

        self._socket_server = await asyncio.start_unix_server(
            self._socket_handler,
            path=str(self.socket_path),
        )
        self._running = True

    async def stop(self) -> None:
        self._running = False
        if self._socket_server:
            self._socket_server.close()
            await self._socket_server.wait_closed()
            self._socket_server = None

        if self.socket_path.exists():
            self.socket_path.unlink()

        for future in self._pending_futures.values():
            if not future.done():
                future.cancel()
        self._pending_futures.clear()

    def list_peers(self) -> list[Peer]:
        peers = []
        machine = socket.gethostname()

        for name, peer_config in self.config.peers.items():
            status = self._get_peer_status(peer_config.tmux_session)
            peers.append(
                Peer(
                    name=name,
                    path=peer_config.path,
                    machine=machine,
                    tmux_session=peer_config.tmux_session,
                    status=status,
                    last_seen=datetime.utcnow() if status != PeerStatus.OFFLINE else None,
                )
            )

        return peers

    def get_peer(self, name: str) -> Peer | None:
        peer_config = self.config.peers.get(name)
        if not peer_config:
            return None

        status = self._get_peer_status(peer_config.tmux_session)
        return Peer(
            name=name,
            path=peer_config.path,
            machine=socket.gethostname(),
            tmux_session=peer_config.tmux_session,
            status=status,
            last_seen=datetime.utcnow() if status != PeerStatus.OFFLINE else None,
        )

    async def send_query(
        self,
        peer_name: str,
        query: str,
        from_peer: str = "repowire",
        timeout: float = 120.0,
    ) -> str:
        peer = self.get_peer(peer_name)
        if not peer:
            raise ValueError(f"Unknown peer: {peer_name}")

        if peer.status == PeerStatus.OFFLINE:
            raise ValueError(f"Peer {peer_name} is offline")

        pane = self._get_peer_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer_name}")

        correlation_id = str(uuid4())
        session_id = self._get_claude_session_id(peer.tmux_session)

        pending_file = self.pending_dir / f"{session_id or correlation_id}.json"
        pending_data = {
            "correlation_id": correlation_id,
            "from_peer": from_peer,
            "to_peer": peer_name,
            "query": query,
            "timestamp": datetime.utcnow().isoformat(),
        }
        pending_file.write_text(json.dumps(pending_data))

        response_future: asyncio.Future[str] = asyncio.Future()
        self._pending_futures[correlation_id] = response_future

        formatted_query = f"@{from_peer} asks: {query}"
        pane.send_keys(formatted_query, enter=True)

        try:
            response = await asyncio.wait_for(response_future, timeout=timeout)
            return response
        except asyncio.TimeoutError:
            raise TimeoutError(f"No response from {peer_name} within {timeout}s")
        finally:
            self._pending_futures.pop(correlation_id, None)
            if pending_file.exists():
                pending_file.unlink()

    async def send_notification(
        self,
        peer_name: str,
        message: str,
        from_peer: str = "repowire",
    ) -> None:
        peer = self.get_peer(peer_name)
        if not peer:
            raise ValueError(f"Unknown peer: {peer_name}")

        if peer.status == PeerStatus.OFFLINE:
            raise ValueError(f"Peer {peer_name} is offline")

        pane = self._get_peer_pane(peer.tmux_session)
        if not pane:
            raise ValueError(f"Could not find pane for peer {peer_name}")

        formatted_message = f"@{from_peer} says: {message}"
        pane.send_keys(formatted_message, enter=True)

    async def broadcast(self, message: str, from_peer: str = "repowire") -> None:
        for peer in self.list_peers():
            if peer.status != PeerStatus.OFFLINE:
                try:
                    await self.send_notification(peer.name, message, from_peer)
                except Exception:
                    pass

    def _get_peer_status(self, tmux_session: str) -> PeerStatus:
        try:
            session = self.server.sessions.get(session_name=tmux_session)
            if session is None:
                return PeerStatus.OFFLINE
            return PeerStatus.ONLINE
        except Exception:
            return PeerStatus.OFFLINE

    def _get_peer_pane(self, tmux_session: str) -> libtmux.Pane | None:
        try:
            session = self.server.sessions.get(session_name=tmux_session)
            if session is None:
                return None
            return session.active_pane
        except Exception:
            return None

    def _get_claude_session_id(self, tmux_session: str) -> str | None:
        try:
            session = self.server.sessions.get(session_name=tmux_session)
            if session is None:
                return None

            pane = session.active_pane
            if pane is None:
                return None

            pane_path = pane.pane_current_path
            if not pane_path:
                return None

            claude_projects = Path.home() / ".claude" / "projects"
            if not claude_projects.exists():
                return None

            path_slug = pane_path.replace("/", "-")
            if path_slug.startswith("-"):
                path_slug = path_slug[1:]

            project_dir = claude_projects / f"-{path_slug}"
            if not project_dir.exists():
                for candidate in claude_projects.iterdir():
                    if candidate.is_dir() and path_slug in candidate.name:
                        project_dir = candidate
                        break
                else:
                    return None

            jsonl_files = sorted(
                project_dir.glob("*.jsonl"), key=lambda p: p.stat().st_mtime, reverse=True
            )
            if jsonl_files:
                return jsonl_files[0].stem

            return None
        except Exception:
            return None

    async def _socket_handler(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
    ) -> None:
        try:
            data = await reader.read(65536)
            if not data:
                return

            message = json.loads(data.decode())
            correlation_id = message.get("correlation_id")
            response = message.get("response")

            if correlation_id and response:
                self._handle_response(correlation_id, response)

            writer.write(b'{"status": "ok"}')
            await writer.drain()
        except Exception:
            pass
        finally:
            writer.close()
            await writer.wait_closed()

    def _handle_response(self, correlation_id: str, response: str) -> None:
        future = self._pending_futures.get(correlation_id)
        if future and not future.done():
            future.set_result(response)
