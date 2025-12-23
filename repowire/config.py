from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field, PrivateAttr


class AgentConfig(BaseModel):
    path: str
    model: str = "claude-sonnet-4-20250514"
    color: str = "white"
    capabilities: list[str] = Field(default_factory=list)
    depends_on: list[str] = Field(default_factory=list)
    description: str = ""


class Settings(BaseModel):
    port_range_start: int = 3001
    blackboard_file: str = ".repowire/blackboard.json"
    log_dir: str = ".repowire/logs"
    git_branch_warnings: bool = True


class RepowireConfig(BaseModel):
    name: str
    agents: dict[str, AgentConfig]
    settings: Settings = Field(default_factory=Settings)

    _base_path: Path = PrivateAttr(default_factory=Path.cwd)

    @classmethod
    def from_yaml(cls, path: Path) -> RepowireConfig:
        with open(path) as f:
            data: dict[str, Any] = yaml.safe_load(f)
        config = cls.model_validate(data)
        config._base_path = path.parent
        return config

    @classmethod
    def find_and_load(cls, start_path: Path | None = None) -> RepowireConfig:
        search_path = start_path or Path.cwd()

        for parent in [search_path, *search_path.parents]:
            config_path = parent / "repowire.yaml"
            if config_path.exists():
                config = cls.from_yaml(config_path)
                return config

        raise FileNotFoundError(f"No repowire.yaml found in {search_path} or any parent directory")

    @property
    def base_path(self) -> Path:
        return self._base_path

    def resolve_agent_path(self, agent_name: str) -> Path:
        agent = self.agents[agent_name]
        return (self._base_path / agent.path).resolve()

    def get_agent_port(self, agent_name: str) -> int:
        agent_names = list(self.agents.keys())
        index = agent_names.index(agent_name)
        return self.settings.port_range_start + index
