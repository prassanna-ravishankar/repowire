from __future__ import annotations

import asyncio
import signal
import subprocess
from pathlib import Path
from typing import Any

from opencode_ai import AsyncOpencode
from rich.console import Console

from repowire.blackboard import Blackboard
from repowire.bus import Message, MessageBus, MessageType
from repowire.config import RepowireConfig
from repowire.mcp.server import MCPServerRunner
from repowire.process import ProcessManager


def get_git_branch(repo_path: Path) -> str | None:
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            cwd=repo_path,
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except (subprocess.TimeoutExpired, FileNotFoundError):
        pass
    return None


def check_git_branches(process_manager: ProcessManager) -> dict[str, str | None]:
    branches = {}
    for name in process_manager.agent_names:
        agent = process_manager.get(name)
        if agent:
            branches[name] = get_git_branch(agent.path)
    return branches


class RepowireDaemon:
    def __init__(self, config: RepowireConfig) -> None:
        self.config = config
        self.console = Console()

        blackboard_path = config.base_path / config.settings.blackboard_file
        self.blackboard = Blackboard(persist_path=blackboard_path)
        self.bus = MessageBus()
        self.process_manager = ProcessManager()

        self._clients: dict[str, AsyncOpencode] = {}
        self._mcp_servers: dict[str, MCPServerRunner] = {}
        self._running = False
        self._shutdown_event = asyncio.Event()

    async def boot(self) -> None:
        self._running = True
        self.console.print(f"[bold blue]Repowire[/] - Booting mesh: {self.config.name}")

        self._setup_signal_handlers()
        await self._register_agents()

        if self.config.settings.git_branch_warnings:
            self._check_git_branches()

        await self._start_opencode_processes()
        await self._wait_for_agents_ready()
        await self._connect_sdk_clients()
        await self._start_mcp_servers()
        await self._setup_message_handlers()

        self.console.print("[bold green]Mesh ready![/]")

    def _check_git_branches(self) -> None:
        branches = check_git_branches(self.process_manager)
        unique_branches = set(b for b in branches.values() if b is not None)

        if len(unique_branches) > 1:
            self.console.print("[bold yellow]Warning: Agents are on different git branches![/]")
            for name, branch in branches.items():
                color = "green" if branch else "red"
                branch_display = branch or "(not a git repo)"
                self.console.print(f"  [{color}]{name}[/]: {branch_display}")
            self.console.print("")

    def _setup_signal_handlers(self) -> None:
        loop = asyncio.get_event_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, lambda: asyncio.create_task(self.shutdown()))

    async def _register_agents(self) -> None:
        for name, agent_config in self.config.agents.items():
            path = self.config.resolve_agent_path(name)
            port = self.config.get_agent_port(name)

            self.process_manager.register(
                name=name,
                path=path,
                port=port,
                config=agent_config,
            )
            self.console.print(f"  Registered [cyan]{name}[/] @ {path} (port {port})")

    async def _start_opencode_processes(self) -> None:
        self.console.print("Starting OpenCode instances...")
        await self.process_manager.start_all()

    async def _wait_for_agents_ready(self) -> None:
        self.console.print("Waiting for agents to be ready...")
        ready = await self.process_manager.wait_for_ready(timeout=60.0)
        if not ready:
            self.console.print("[bold red]Warning: Some agents did not become ready[/]")

    async def _connect_sdk_clients(self) -> None:
        for name in self.process_manager.agent_names:
            agent = self.process_manager.get(name)
            if agent:
                client = AsyncOpencode(base_url=agent.base_url)
                self._clients[name] = client
                self.console.print(f"  Connected SDK client for [cyan]{name}[/]")

    async def _start_mcp_servers(self) -> None:
        for name in self.process_manager.agent_names:
            agent = self.process_manager.get(name)
            if agent:
                mcp_server = MCPServerRunner(
                    agent_name=name,
                    port=agent.port,
                    bus=self.bus,
                    blackboard=self.blackboard,
                    process_manager=self.process_manager,
                )
                self._mcp_servers[name] = mcp_server
                self.console.print(f"  MCP server for [cyan]{name}[/] @ port {agent.port + 1000}")

    async def _setup_message_handlers(self) -> None:
        for name in self.process_manager.agent_names:
            handler = self._create_message_handler(name)
            self.bus.register_handler(name, handler)

    def _create_message_handler(self, agent_name: str):
        async def handler(message: Message) -> Any:
            client = self._clients.get(agent_name)
            if not client:
                return None

            sessions = await client.session.list()
            if not sessions:
                session = await client.session.create()
                session_id = session.id
            else:
                session_id = sessions[0].id

            if message.type == MessageType.QUERY:
                prompt = f"[PEER QUERY from {message.source}]: {message.content}\nPlease respond with the requested information."
                response = await client.session.chat(
                    session_id, parts=[{"type": "text", "text": prompt}]
                )

                response_msg = Message.response(
                    source=agent_name,
                    target=message.source,
                    content=self._extract_response_text(response),
                    correlation_id=message.correlation_id or message.id,
                )
                await self.bus.send(response_msg)

            elif message.type == MessageType.NOTIFICATION:
                prompt = f"[NOTIFICATION from {message.source}]: {message.content}"
                await client.session.chat(session_id, parts=[{"type": "text", "text": prompt}])

            elif message.type == MessageType.BROADCAST:
                prompt = f"[BROADCAST from {message.source}]: {message.content}"
                await client.session.chat(session_id, parts=[{"type": "text", "text": prompt}])

        return handler

    def _extract_response_text(self, response: Any) -> str:
        if hasattr(response, "parts"):
            for part in response.parts:
                if hasattr(part, "text"):
                    return part.text
        if hasattr(response, "text"):
            return response.text
        return str(response)

    async def send_to_agent(self, agent_name: str, message: str) -> str | None:
        client = self._clients.get(agent_name)
        if not client:
            return None

        sessions = await client.session.list()
        if not sessions:
            session = await client.session.create()
            session_id = session.id
        else:
            session_id = sessions[0].id

        response = await client.session.chat(session_id, parts=[{"type": "text", "text": message}])
        return self._extract_response_text(response)

    async def broadcast_to_all(self, message: str) -> dict[str, str]:
        results = {}
        for name in self.process_manager.agent_names:
            response = await self.send_to_agent(name, message)
            if response:
                results[name] = response
        return results

    async def shutdown(self) -> None:
        if not self._running:
            return

        self._running = False
        self.console.print("\n[bold yellow]Shutting down...[/]")

        for name, mcp_server in self._mcp_servers.items():
            await mcp_server.stop()

        for name, client in self._clients.items():
            await client.close()

        await self.process_manager.stop_all()

        self._shutdown_event.set()
        self.console.print("[bold green]Shutdown complete.[/]")

    async def wait_for_shutdown(self) -> None:
        await self._shutdown_event.wait()

    async def run(self) -> None:
        await self.boot()
        await self.wait_for_shutdown()


async def run_daemon(config_path: Path | None = None) -> None:
    if config_path:
        config = RepowireConfig.from_yaml(config_path)
    else:
        config = RepowireConfig.find_and_load()

    daemon = RepowireDaemon(config)
    await daemon.run()
