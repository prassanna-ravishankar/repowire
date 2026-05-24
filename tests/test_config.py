import tempfile
from pathlib import Path
from unittest.mock import patch

import pytest
from click.testing import CliRunner
from pydantic import ValidationError

from repowire.config.models import (
    AgentType,
    Config,
    DaemonConfig,
    LoggingConfig,
    PeerConfig,
    RelayConfig,
    SpawnProfile,
    SpawnSettings,
    UpdatesConfig,
    apply_spawn_profile,
    load_config,
)


class TestConfig:
    def test_default_config(self):
        config = Config()

        assert config.relay.enabled is False
        assert config.relay.url == "wss://repowire.io"
        assert len(config.peers) == 0
        assert config.updates.check_enabled is False
        assert config.experiments.sqlite_state is True

    def test_get_peer(self):
        config = Config(
            peers={
                "backend": PeerConfig(name="backend", tmux_session="test", path="/test"),
            }
        )
        assert config.get_peer("backend") is not None
        assert config.get_peer("backend").name == "backend"
        assert config.get_peer("nonexistent") is None

    def test_extra_fields_ignored(self):
        """Config should ignore unknown fields (e.g., removed 'opencode' section)."""
        config = Config(opencode={"default_url": "http://localhost:4096"})
        assert not hasattr(config, "opencode")

    def test_peer_config_extra_fields_ignored(self):
        """PeerConfig should ignore removed fields like opencode_url, session_id."""
        peer = PeerConfig(
            name="test",
            opencode_url="http://localhost:4096",
            session_id="abc123",
        )
        assert peer.name == "test"

    def test_save_and_load(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            config_path = Path(tmpdir) / "config.yaml"
            with patch.object(Config, "get_config_dir", return_value=Path(tmpdir)), \
                 patch.object(Config, "get_config_path", return_value=config_path):
                cfg = Config(daemon=DaemonConfig(port=9999))
                cfg.save()

                loaded = load_config()
                assert loaded.daemon.port == 9999

    def test_load_config_with_env(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            with patch.object(Config, "get_config_path", return_value=Path(tmpdir) / "config.yaml"):
                with patch.dict(
                    "os.environ",
                    {
                        "REPOWIRE_RELAY_URL": "wss://custom.relay.io",
                        "REPOWIRE_API_KEY": "rw_test123",
                    },
                ):
                    config = load_config()

                    assert config.relay.url == "wss://custom.relay.io"
                    assert config.relay.api_key == "rw_test123"
                    assert config.relay.enabled is True

    def test_setup_http_mcp_helper_generates_local_token(self):
        from repowire.cli import _enable_http_mcp

        config = Config()
        changed = _enable_http_mcp(config)

        assert changed is True
        assert config.daemon.mcp_http.enabled is True
        assert config.daemon.auth_token is not None
        assert config.daemon.auth_token.startswith("rw_local_")
        assert len(config.daemon.auth_token) > 40

    def test_setup_http_mcp_helper_preserves_existing_token(self):
        from repowire.cli import _enable_http_mcp

        config = Config(daemon=DaemonConfig(auth_token="existing-secret"))
        _enable_http_mcp(config)

        assert config.daemon.mcp_http.enabled is True
        assert config.daemon.auth_token == "existing-secret"

    def test_setup_http_mcp_flag_writes_config_without_detected_agents(self, tmp_path, monkeypatch):
        from repowire.cli import main

        config_path = tmp_path / "config.yaml"
        monkeypatch.setattr(Config, "get_config_dir", classmethod(lambda cls: tmp_path))
        monkeypatch.setattr(Config, "get_config_path", classmethod(lambda cls: config_path))
        monkeypatch.setattr("repowire.cli._cleanup_legacy_artifacts", lambda: None)
        monkeypatch.setattr("shutil.which", lambda _name: None)

        result = CliRunner().invoke(
            main,
            ["setup", "--http-mcp", "--non-interactive", "--no-service"],
        )

        assert result.exit_code == 0
        loaded = load_config()
        assert loaded.daemon.mcp_http.enabled is True
        assert loaded.daemon.auth_token is not None
        assert loaded.daemon.auth_token.startswith("rw_local_")

    def test_setup_update_checks_flag_writes_config_without_detected_agents(
        self,
        tmp_path,
        monkeypatch,
    ):
        from repowire.cli import main

        config_path = tmp_path / "config.yaml"
        monkeypatch.setattr(Config, "get_config_dir", classmethod(lambda cls: tmp_path))
        monkeypatch.setattr(Config, "get_config_path", classmethod(lambda cls: config_path))
        monkeypatch.setattr("repowire.cli._cleanup_legacy_artifacts", lambda: None)
        monkeypatch.setattr("shutil.which", lambda _name: None)

        result = CliRunner().invoke(
            main,
            ["setup", "--update-checks", "--non-interactive", "--no-service"],
        )

        assert result.exit_code == 0
        loaded = load_config()
        assert loaded.updates.check_enabled is True


class TestUpdatesConfig:
    def test_default_off(self):
        assert UpdatesConfig().check_enabled is False

    def test_can_enable_update_checks(self):
        assert UpdatesConfig(check_enabled=True).check_enabled is True


class TestUpdateCommandHelpers:
    def test_package_spec_without_extras(self):
        from repowire.cli import _repowire_package_spec

        assert _repowire_package_spec(Config()) == "repowire"

    def test_package_spec_includes_acp_extra_when_experiment_enabled(self):
        from repowire.cli import _repowire_package_spec

        config = Config(experiments={"acp_broker_client": True})
        assert _repowire_package_spec(config) == "repowire[acp]"

    def test_upgrade_command_preserves_acp_extra_for_uv(self):
        from repowire.cli import _upgrade_command

        config = Config(experiments={"acp_broker_client": True})
        assert _upgrade_command("uv", config) == [
            "uv",
            "tool",
            "install",
            "repowire[acp]",
            "--upgrade",
            "--force",
        ]

    def test_upgrade_command_uses_normal_upgrade_without_extras(self):
        from repowire.cli import _upgrade_command

        assert _upgrade_command("pipx", Config()) == ["pipx", "upgrade", "repowire"]


class TestRelayConfig:
    def test_dashboard_url_without_key(self):
        relay = RelayConfig()
        assert relay.dashboard_url is None

    def test_dashboard_url_with_key(self):
        relay = RelayConfig(api_key="rw_test")
        assert relay.dashboard_url == "https://repowire.io/dashboard"

    def test_default_url(self):
        relay = RelayConfig()
        assert relay.url == "wss://repowire.io"


class TestSpawnSettings:
    def test_defaults_empty(self):
        spawn = SpawnSettings()
        assert spawn.commands == {}
        assert spawn.allowed_commands == []
        assert spawn.allowed_paths == []

    def test_with_values(self):
        spawn = SpawnSettings(
            commands={AgentType.CLAUDE_CODE: "claude", AgentType.OPENCODE: "opencode"},
            allowed_paths=["~/git"],
        )
        assert len(spawn.commands) == 2
        assert "~/git" in spawn.allowed_paths

    def test_legacy_allowed_commands_bootstrap_commands(self):
        spawn = SpawnSettings(allowed_commands=["claude --model opus", "codex"])
        assert spawn.commands[AgentType.CLAUDE_CODE] == "claude --model opus"
        assert spawn.commands[AgentType.CODEX] == "codex"

    def test_profiles_can_extend_backend_commands(self):
        spawn = SpawnSettings(
            commands={AgentType.CODEX: "codex --dangerously-bypass-approvals-and-sandbox"},
            profiles={
                AgentType.CODEX: {
                    "fast": SpawnProfile(args=["--model", "gpt-5-mini"]),
                },
            },
        )

        assert apply_spawn_profile(
            spawn.commands[AgentType.CODEX],
            spawn.profiles[AgentType.CODEX]["fast"],
        ) == "codex --dangerously-bypass-approvals-and-sandbox --model gpt-5-mini"

    def test_profile_args_are_shell_quoted(self):
        assert apply_spawn_profile(
            "claude --dangerously-skip-permissions",
            SpawnProfile(args=["--model", "claude sonnet"]),
        ) == "claude --dangerously-skip-permissions --model 'claude sonnet'"


class TestAgentType:
    def test_claude_code(self):
        assert AgentType.CLAUDE_CODE == "claude-code"

    def test_opencode(self):
        assert AgentType.OPENCODE == "opencode"

    def test_from_string(self):
        assert AgentType("claude-code") == AgentType.CLAUDE_CODE


class TestPeerConfigEffective:
    def test_effective_name_with_display(self):
        peer = PeerConfig(name="legacy", display_name="modern")
        assert peer.effective_name == "modern"

    def test_effective_name_fallback(self):
        peer = PeerConfig(name="legacy")
        assert peer.effective_name == "legacy"

    def test_effective_peer_id_with_id(self):
        peer = PeerConfig(name="test", peer_id="repow-dev-abc")
        assert peer.effective_peer_id == "repow-dev-abc"

    def test_effective_peer_id_legacy(self):
        peer = PeerConfig(name="test", tmux_session="0:test")
        assert peer.effective_peer_id == "legacy-0:test"


class TestLoggingConfig:
    def test_default_level(self):
        assert LoggingConfig().level == "info"

    @pytest.mark.parametrize("level", ["debug", "info", "warning", "error", "critical"])
    def test_valid_levels(self, level: str):
        assert LoggingConfig(level=level).level == level

    @pytest.mark.parametrize("level", ["DEBUG", "Info", "WARNING", "Error", "CRITICAL"])
    def test_case_insensitive_normalized_to_lower(self, level: str):
        assert LoggingConfig(level=level).level == level.lower()

    @pytest.mark.parametrize("level", ["trace", "verbose", "", "warn", "fatal", "info "])
    def test_invalid_level_raises(self, level: str):
        with pytest.raises(ValidationError) as exc:
            LoggingConfig(level=level)
        # Pydantic wraps the ValueError; check the message surfaces the user's input
        assert level in str(exc.value) or repr(level) in str(exc.value)

    def test_invalid_level_message_lists_valid_options(self):
        with pytest.raises(ValidationError) as exc:
            LoggingConfig(level="loud")
        msg = str(exc.value)
        for valid in ["debug", "info", "warning", "error", "critical"]:
            assert valid in msg
