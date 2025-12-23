from __future__ import annotations

import asyncio
import signal
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from repowire.config import AgentConfig


@dataclass
class AgentProcess:
    name: str
    path: Path
    port: int
    config: AgentConfig
    process: asyncio.subprocess.Process | None = None
    _output_queue: asyncio.Queue[str] = field(default_factory=asyncio.Queue)

    @property
    def is_running(self) -> bool:
        return self.process is not None and self.process.returncode is None

    @property
    def base_url(self) -> str:
        return f"http://localhost:{self.port}"

    async def start(self) -> None:
        if self.is_running:
            return

        self.path.mkdir(parents=True, exist_ok=True)

        self.process = await asyncio.create_subprocess_exec(
            "opencode",
            "--port",
            str(self.port),
            cwd=str(self.path),
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
            start_new_session=True,
        )

        asyncio.create_task(self._read_output())

    async def _read_output(self) -> None:
        if not self.process or not self.process.stdout:
            return

        while True:
            line = await self.process.stdout.readline()
            if not line:
                break
            decoded = line.decode().rstrip()
            await self._output_queue.put(decoded)

    async def get_output(self, timeout: float = 0.1) -> str | None:
        try:
            return await asyncio.wait_for(self._output_queue.get(), timeout=timeout)
        except asyncio.TimeoutError:
            return None

    async def stop(self) -> None:
        if not self.process:
            return

        try:
            self.process.send_signal(signal.SIGTERM)
            await asyncio.wait_for(self.process.wait(), timeout=5.0)
        except asyncio.TimeoutError:
            self.process.kill()
            await self.process.wait()
        finally:
            self.process = None


class ProcessManager:
    def __init__(self) -> None:
        self._agents: dict[str, AgentProcess] = {}

    def register(
        self,
        name: str,
        path: Path,
        port: int,
        config: AgentConfig,
    ) -> AgentProcess:
        agent = AgentProcess(name=name, path=path, port=port, config=config)
        self._agents[name] = agent
        return agent

    def get(self, name: str) -> AgentProcess | None:
        return self._agents.get(name)

    def all(self) -> list[AgentProcess]:
        return list(self._agents.values())

    @property
    def agent_names(self) -> list[str]:
        return list(self._agents.keys())

    async def start_all(self) -> None:
        await asyncio.gather(*[agent.start() for agent in self._agents.values()])

    async def stop_all(self) -> None:
        await asyncio.gather(*[agent.stop() for agent in self._agents.values()])

    async def wait_for_ready(self, timeout: float = 30.0) -> bool:
        import httpx

        async def check_agent(agent: AgentProcess) -> bool:
            deadline = asyncio.get_event_loop().time() + timeout
            async with httpx.AsyncClient() as client:
                while asyncio.get_event_loop().time() < deadline:
                    try:
                        resp = await client.get(f"{agent.base_url}/app", timeout=1.0)
                        if resp.status_code == 200:
                            return True
                    except (httpx.ConnectError, httpx.TimeoutException):
                        pass
                    await asyncio.sleep(0.5)
            return False

        results = await asyncio.gather(*[check_agent(a) for a in self._agents.values()])
        return all(results)
