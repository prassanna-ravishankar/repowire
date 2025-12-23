from __future__ import annotations

import asyncio
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any

from mcp.server.fastmcp import Context, FastMCP

if TYPE_CHECKING:
    from repowire.blackboard import Blackboard
    from repowire.bus import MessageBus
    from repowire.process import ProcessManager


@dataclass
class RepowireContext:
    agent_name: str
    bus: MessageBus
    blackboard: Blackboard
    process_manager: ProcessManager


def create_mcp_server(
    agent_name: str,
    bus: MessageBus,
    blackboard: Blackboard,
    process_manager: ProcessManager,
) -> FastMCP:
    from repowire.bus import Message

    mcp = FastMCP(
        f"repowire-{agent_name}",
        json_response=True,
    )

    ctx = RepowireContext(
        agent_name=agent_name,
        bus=bus,
        blackboard=blackboard,
        process_manager=process_manager,
    )

    @mcp.tool()
    def list_peers() -> dict[str, Any]:
        peers = []
        for name in process_manager.agent_names:
            if name != agent_name:
                agent = process_manager.get(name)
                if agent:
                    peers.append(
                        {
                            "name": name,
                            "path": str(agent.path),
                            "is_running": agent.is_running,
                            "capabilities": agent.config.capabilities,
                            "description": agent.config.description,
                        }
                    )
        return {"peers": peers, "self": agent_name}

    @mcp.tool()
    async def ask_peer(target: str, query: str) -> str:
        if target not in process_manager.agent_names:
            return f"Error: Unknown peer '{target}'. Use list_peers() to see available peers."

        if target == agent_name:
            return "Error: Cannot query yourself. Use ask_peer for other agents."

        message = Message.query(source=agent_name, target=target, content=query)

        try:
            response = await bus.query(message, timeout=60.0)
            return str(response.content)
        except asyncio.TimeoutError:
            return f"Error: Peer '{target}' did not respond within 60 seconds."

    @mcp.tool()
    async def notify_peer(target: str, message: str) -> str:
        if target not in process_manager.agent_names:
            return f"Error: Unknown peer '{target}'. Use list_peers() to see available peers."

        msg = Message.notification(source=agent_name, target=target, content=message)
        await bus.send(msg)
        return f"Notification sent to '{target}'."

    @mcp.tool()
    async def broadcast_update(message: str) -> str:
        msg = Message.broadcast(source=agent_name, content=message)
        await bus.send(msg)
        peers_count = len(process_manager.agent_names) - 1
        return f"Broadcast sent to {peers_count} peers."

    @mcp.tool()
    def read_peer_file(target: str, file_path: str) -> str:
        if target not in process_manager.agent_names:
            return f"Error: Unknown peer '{target}'. Use list_peers() to see available peers."

        agent = process_manager.get(target)
        if not agent:
            return f"Error: Peer '{target}' not found."

        full_path = agent.path / file_path

        if not full_path.exists():
            return f"Error: File '{file_path}' does not exist in {target}'s repository."

        if not full_path.is_file():
            return f"Error: '{file_path}' is not a file."

        try:
            resolved = full_path.resolve()
            if not str(resolved).startswith(str(agent.path.resolve())):
                return "Error: Path traversal not allowed."

            content = resolved.read_text()
            if len(content) > 100000:
                content = content[:100000] + "\n... (truncated, file too large)"
            return content
        except Exception as e:
            return f"Error reading file: {e}"

    @mcp.tool()
    async def write_blackboard(key: str, value: str) -> str:
        await blackboard.write(key, value, agent_name)
        return f"Blackboard key '{key}' updated."

    @mcp.tool()
    async def read_blackboard(key: str | None = None) -> dict[str, Any] | str:
        if key is None:
            data = await blackboard.read_all()
            return {"blackboard": data}

        value = await blackboard.read(key)
        if value is None:
            return f"Key '{key}' not found in blackboard."
        return str(value)

    @mcp.tool()
    def get_peer_status() -> dict[str, Any]:
        statuses = {}
        for name in process_manager.agent_names:
            agent = process_manager.get(name)
            if agent:
                statuses[name] = {
                    "is_running": agent.is_running,
                    "port": agent.port,
                    "path": str(agent.path),
                    "is_self": name == agent_name,
                }
        return {"agents": statuses}

    return mcp


class MCPServerRunner:
    def __init__(
        self,
        agent_name: str,
        port: int,
        bus: MessageBus,
        blackboard: Blackboard,
        process_manager: ProcessManager,
    ) -> None:
        self.agent_name = agent_name
        self.port = port
        self.mcp = create_mcp_server(agent_name, bus, blackboard, process_manager)
        self._server_task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        self._server_task = asyncio.create_task(self._run_server())

    async def _run_server(self) -> None:
        self.mcp.run(transport="streamable-http", host="127.0.0.1", port=self.port + 1000)

    async def stop(self) -> None:
        if self._server_task:
            self._server_task.cancel()
            try:
                await self._server_task
            except asyncio.CancelledError:
                pass
