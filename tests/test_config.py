import os
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
    RemoteToolApprovalConfig,
    SkillsConfig,
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

    def test_bare_config_is_env_and_yaml_insensitive(self):
        # Critical invariant (codex review): Config() is a pure value object —
        # constructing it must NOT read ambient env or the on-disk yaml file,
        # so the 24 call sites + fixtures that build Configs in-memory are safe.
        with patch.dict(
            "os.environ",
            {"REPOWIRE_RELAY_URL": "wss://leak.io", "REPOWIRE_DAEMON__PORT": "9999"},
        ):
            c = Config()
            assert c.relay.url == "wss://repowire.io"  # default, not the env value
            assert c.daemon.port == 8377

    def test_load_config_nested_env_delimiter(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            with patch.object(Config, "get_config_path", return_value=Path(tmpdir) / "config.yaml"):
                with patch.dict("os.environ", {"REPOWIRE_DAEMON__PORT": "9100"}):
                    assert load_config().daemon.port == 9100

    def test_nested_relay_api_key_enables_relay(self):
        # codex review: the relay.enabled side-effect must fire for the canonical
        # nested REPOWIRE_RELAY__API_KEY spelling, not just the flat alias —
        # otherwise adopting nested env leaves the relay configured-but-disabled.
        with tempfile.TemporaryDirectory() as tmpdir:
            with patch.object(Config, "get_config_path", return_value=Path(tmpdir) / "config.yaml"):
                with patch.dict(
                    "os.environ", {"REPOWIRE_RELAY__API_KEY": "nested-key"}, clear=True
                ):
                    config = load_config()
                    assert config.relay.api_key == "nested-key"
                    assert config.relay.enabled is True

    def test_load_config_env_overrides_yaml(self):
        # Intentional behavior change: env now overrides the yaml file (previously
        # env was applied only when no config file existed).
        with tempfile.TemporaryDirectory() as tmpdir:
            cfgp = Path(tmpdir) / "config.yaml"
            with patch.object(Config, "get_config_dir", return_value=Path(tmpdir)), \
                 patch.object(Config, "get_config_path", return_value=cfgp):
                Config(daemon=DaemonConfig(port=7000), relay=RelayConfig(url="wss://yaml.io")).save()
                # No env -> yaml wins
                with patch.dict("os.environ", {}, clear=False):
                    for k in [k for k in os.environ if k.startswith("REPOWIRE_")]:
                        os.environ.pop(k)
                    assert load_config().daemon.port == 7000
                # Env present -> env wins over yaml
                with patch.dict(
                    "os.environ",
                    {"REPOWIRE_DAEMON__PORT": "8500", "REPOWIRE_RELAY_URL": "wss://env.io"},
                ):
                    c = load_config()
                    assert c.daemon.port == 8500
                    assert c.relay.url == "wss://env.io"

    def test_spawn_commands_profiles_roundtrip(self):
        # codex review C: enum-keyed spawn commands/profiles must survive
        # save() -> load() through the settings loader unchanged.
        with tempfile.TemporaryDirectory() as tmpdir:
            cfgp = Path(tmpdir) / "config.yaml"
            with patch.object(Config, "get_config_dir", return_value=Path(tmpdir)), \
                 patch.object(Config, "get_config_path", return_value=cfgp):
                cfg = Config(
                    daemon=DaemonConfig(
                        spawn=SpawnSettings(
                            commands={AgentType.CODEX: "codex --x"},
                            allowed_paths=["~/git"],
                        )
                    )
                )
                cfg.save()
                for k in [k for k in os.environ if k.startswith("REPOWIRE_")]:
                    os.environ.pop(k)
                loaded = load_config()
                assert loaded.daemon.spawn.commands.get(AgentType.CODEX) == "codex --x"
                assert loaded.daemon.spawn.allowed_paths == ["~/git"]

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


class TestRemoteToolApprovalConfig:
    def test_defaults_gate_mutating_tools_only(self):
        cfg = RemoteToolApprovalConfig()
        assert cfg.enabled is False
        assert "Bash" in cfg.gated_tools
        assert not (set(cfg.gated_tools) & {"Read", "Glob", "Grep", "LS"})

    def test_read_only_tools_are_filtered_out(self):
        cfg = RemoteToolApprovalConfig(gated_tools=["Bash", "Read", "Grep", "Edit"])
        assert cfg.gated_tools == ["Bash", "Edit"]

    @pytest.mark.parametrize("bad", [0, -1, -45.0])
    def test_non_positive_timeout_falls_back_to_default(self, bad: float):
        assert RemoteToolApprovalConfig(timeout_seconds=bad).timeout_seconds == 45.0

    def test_positive_timeout_is_kept(self):
        assert RemoteToolApprovalConfig(timeout_seconds=10.0).timeout_seconds == 10.0


class TestSkillsConfig:
    def test_defaults_are_all_none(self):
        cfg = SkillsConfig()
        assert cfg.default_backend is None
        assert cfg.default_reviewer_backend is None
        assert cfg.resolve("default_reviewer_backend") is None

    def test_resolve_prefers_per_skill_over_generic(self):
        cfg = SkillsConfig(default_backend="claude-code", default_reviewer_backend="codex")
        assert cfg.resolve("default_reviewer_backend") == "codex"

    def test_resolve_falls_back_to_default_backend(self):
        cfg = SkillsConfig(default_backend="gemini")
        assert cfg.resolve("default_planner_backend") == "gemini"

    def test_config_exposes_skills_section(self):
        assert isinstance(Config().skills, SkillsConfig)


class TestConfigGetCommand:
    def _invoke(self, monkeypatch, key, cfg, extra=None):
        from repowire.cli import main

        monkeypatch.setattr("repowire.config.models.load_config", lambda: cfg)
        return CliRunner().invoke(main, ["config", "get", key, *(extra or [])])

    def test_prints_a_set_value(self, monkeypatch):
        cfg = Config(skills=SkillsConfig(default_reviewer_backend="codex"))
        result = self._invoke(monkeypatch, "skills.default_reviewer_backend", cfg)
        assert result.exit_code == 0
        assert result.output.strip() == "codex"

    def test_unset_value_prints_nothing_exit_zero(self, monkeypatch):
        result = self._invoke(monkeypatch, "skills.default_reviewer_backend", Config())
        assert result.exit_code == 0
        assert result.output.strip() == ""

    def test_unknown_key_exits_2(self, monkeypatch):
        result = self._invoke(monkeypatch, "skills.nope", Config())
        assert result.exit_code == 2

    def test_nested_non_skills_key(self, monkeypatch):
        result = self._invoke(monkeypatch, "daemon.port", Config())
        assert result.exit_code == 0
        assert result.output.strip() != ""  # daemon.port has a default

    def test_skill_key_resolves_generic_default_backend(self, monkeypatch):
        # Per-skill key unset but default_backend set → the seam resolves it
        # (regression: raw attr traversal returned empty here).
        cfg = Config(skills=SkillsConfig(default_backend="codex"))
        result = self._invoke(monkeypatch, "skills.default_reviewer_backend", cfg)
        assert result.exit_code == 0
        assert result.output.strip() == "codex"

    def test_skill_key_per_skill_wins_over_generic(self, monkeypatch):
        cfg = Config(skills=SkillsConfig(default_backend="codex", default_planner_backend="gemini"))
        result = self._invoke(monkeypatch, "skills.default_planner_backend", cfg)
        assert result.output.strip() == "gemini"
