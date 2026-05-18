"""Unit tests for repowire.peer_mcp backend dispatch.

These tests exercise the pure-function layer: mock subprocess for claude-code,
redirect codex/gemini config paths into tmp_path.
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from unittest.mock import patch

import pytest

from repowire import peer_mcp
from repowire.config.models import AgentType
from repowire.protocol.peers import Peer


def _peer(backend: AgentType, path: str = "/repo") -> Peer:
    return Peer(
        peer_id="repow-test-aaaaaaaa",
        display_name="test",
        path=path,
        machine="thishost",
        backend=backend,
    )


# ---------------------------------------------------------------------------
# claude-code
# ---------------------------------------------------------------------------


def _claude_result(stdout: str = "", stderr: str = "", returncode: int = 0):
    return subprocess.CompletedProcess(
        args=["claude"], returncode=returncode, stdout=stdout, stderr=stderr,
    )


class TestClaudeCode:
    def test_list_empty(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        empty_result = _claude_result("No MCP servers configured.", returncode=1)
        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", return_value=empty_result):
            assert peer_mcp.list_servers(peer) == []

    def test_list_parses_stdio_and_http(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        stdout = (
            "filesystem: npx @modelcontextprotocol/server-filesystem /tmp - ✓ Connected\n"
            "remote: https://example.com/mcp - ✓ Connected\n"
        )
        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", return_value=_claude_result(stdout)):
            servers = peer_mcp.list_servers(peer)
        assert len(servers) == 2
        fs = next(s for s in servers if s.name == "filesystem")
        assert fs.type == "stdio"
        assert fs.command == "npx"
        assert fs.args == ["@modelcontextprotocol/server-filesystem", "/tmp"]
        remote = next(s for s in servers if s.name == "remote")
        assert remote.type == "http"
        assert remote.url == "https://example.com/mcp"

    def test_add_stdio_user_scope(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        calls: list[list[str]] = []

        def fake_run(argv, **kw):
            calls.append(argv)
            if "list" in argv:
                return _claude_result("No MCP servers configured.", returncode=1)
            return _claude_result("Added.")

        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", side_effect=fake_run):
            peer_mcp.add_server(peer, peer_mcp.McpServerSpec(
                name="mytool", command="mybin", args=["--flag"], scope="user",
            ))

        add_call = next(c for c in calls if "add" in c)
        assert "-s" in add_call and "user" in add_call
        assert "mytool" in add_call
        assert "mybin" in add_call
        assert "--flag" in add_call

    def test_add_duplicate_rejected(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        existing = _claude_result("foo: bar - ✓ Connected\n")
        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", return_value=existing):
            with pytest.raises(peer_mcp.DuplicateServerError):
                peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="foo", command="bar"))

    def test_remove(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        calls: list[list[str]] = []

        def fake_run(argv, **kw):
            calls.append(argv)
            if "list" in argv:
                return _claude_result("foo: bar - ✓ Connected\n")
            return _claude_result("Removed.")

        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", side_effect=fake_run):
            peer_mcp.remove_server(peer, "foo")

        rm_call = next(c for c in calls if "remove" in c)
        assert "foo" in rm_call

    def test_timeout_surfaces(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        timeout = subprocess.TimeoutExpired(cmd="claude", timeout=10)
        with patch("repowire.peer_mcp.shutil.which", return_value="/usr/bin/claude"), \
             patch("repowire.peer_mcp.subprocess.run", side_effect=timeout):
            with pytest.raises(peer_mcp.BackendTimeoutError):
                peer_mcp.list_servers(peer)

    def test_cli_not_on_path(self):
        peer = _peer(AgentType.CLAUDE_CODE)
        with patch("repowire.peer_mcp.shutil.which", return_value=None):
            with pytest.raises(peer_mcp.NotSupportedError):
                peer_mcp.list_servers(peer)


# ---------------------------------------------------------------------------
# codex
# ---------------------------------------------------------------------------


@pytest.fixture
def codex_config(tmp_path: Path, monkeypatch):
    cfg = tmp_path / "config.toml"
    monkeypatch.setattr(peer_mcp, "CODEX_CONFIG_PATH", cfg)
    return cfg


class TestCodex:
    def test_list_empty_no_file(self, codex_config):
        peer = _peer(AgentType.CODEX)
        assert peer_mcp.list_servers(peer) == []

    def test_add_list_remove_cycle(self, codex_config):
        peer = _peer(AgentType.CODEX)
        peer_mcp.add_server(peer, peer_mcp.McpServerSpec(
            name="repowire", command="repowire", args=["mcp"], env={"FOO": "bar"},
        ))
        assert codex_config.exists()
        body = codex_config.read_text()
        assert "[mcp_servers.repowire]" in body
        assert 'command = "repowire"' in body
        assert 'args = ["mcp"]' in body
        assert "FOO" in body

        servers = peer_mcp.list_servers(peer)
        assert len(servers) == 1
        assert servers[0].name == "repowire"
        assert servers[0].command == "repowire"
        assert servers[0].args == ["mcp"]
        assert servers[0].env_keys == ["FOO"]

        peer_mcp.remove_server(peer, "repowire")
        assert "[mcp_servers.repowire]" not in codex_config.read_text()

    def test_add_preserves_existing(self, codex_config):
        codex_config.write_text("[other]\nkey = \"val\"\n")
        peer = _peer(AgentType.CODEX)
        peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="new", command="x"))
        body = codex_config.read_text()
        assert "[other]" in body
        assert "[mcp_servers.new]" in body

    def test_remove_unknown(self, codex_config):
        codex_config.write_text("")
        peer = _peer(AgentType.CODEX)
        with pytest.raises(peer_mcp.ServerNotFoundError):
            peer_mcp.remove_server(peer, "nope")


# ---------------------------------------------------------------------------
# gemini
# ---------------------------------------------------------------------------


@pytest.fixture
def gemini_config(tmp_path: Path, monkeypatch):
    cfg = tmp_path / "settings.json"
    monkeypatch.setattr(peer_mcp, "GEMINI_SETTINGS_PATH", cfg)
    return cfg


class TestGemini:
    def test_list_empty_no_file(self, gemini_config):
        peer = _peer(AgentType.GEMINI)
        assert peer_mcp.list_servers(peer) == []

    def test_add_list_remove_cycle(self, gemini_config):
        peer = _peer(AgentType.GEMINI)
        peer_mcp.add_server(peer, peer_mcp.McpServerSpec(
            name="repowire", command="repowire", args=["mcp"], env={"TOKEN": "secret"},
        ))
        data = json.loads(gemini_config.read_text())
        assert "repowire" in data["mcpServers"]
        assert data["mcpServers"]["repowire"]["command"] == "repowire"

        servers = peer_mcp.list_servers(peer)
        assert len(servers) == 1
        assert servers[0].env_keys == ["TOKEN"]

        peer_mcp.remove_server(peer, "repowire")
        data = json.loads(gemini_config.read_text())
        assert data.get("mcpServers", {}) == {}

    def test_duplicate_rejected(self, gemini_config):
        peer = _peer(AgentType.GEMINI)
        peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="foo", command="x"))
        with pytest.raises(peer_mcp.DuplicateServerError):
            peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="foo", command="y"))

    def test_empty_name_rejected(self, gemini_config):
        peer = _peer(AgentType.GEMINI)
        with pytest.raises(peer_mcp.BackendError):
            peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="", command="x"))


# ---------------------------------------------------------------------------
# unsupported backend
# ---------------------------------------------------------------------------


class TestUnsupported:
    def test_opencode_not_supported(self):
        peer = _peer(AgentType.OPENCODE)
        with pytest.raises(peer_mcp.NotSupportedError):
            peer_mcp.list_servers(peer)
        with pytest.raises(peer_mcp.NotSupportedError):
            peer_mcp.add_server(peer, peer_mcp.McpServerSpec(name="x", command="y"))

    def test_pi_not_supported(self):
        peer = _peer(AgentType.PI)
        with pytest.raises(peer_mcp.NotSupportedError):
            peer_mcp.list_servers(peer)
