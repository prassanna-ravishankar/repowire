import pytest
import tempfile
from pathlib import Path
from unittest.mock import patch

from repowire.config.models import Config, RelayConfig, PeerConfig, load_config


class TestConfig:
    def test_default_config(self):
        config = Config()

        assert config.relay.enabled is False
        assert config.relay.url == "wss://relay.repowire.io"
        assert len(config.peers) == 0

    def test_add_peer(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            with patch.object(Config, "get_config_dir", return_value=Path(tmpdir)):
                config = Config()
                config.add_peer("backend", "claude-backend", "/app/backend")

                assert "backend" in config.peers
                assert config.peers["backend"].tmux_session == "claude-backend"
                assert config.peers["backend"].path == "/app/backend"

    def test_remove_peer(self):
        config = Config(
            peers={
                "backend": PeerConfig(tmux_session="test", path="/test"),
            }
        )

        with tempfile.TemporaryDirectory() as tmpdir:
            with patch.object(Config, "get_config_dir", return_value=Path(tmpdir)):
                result = config.remove_peer("backend")
                assert result is True
                assert "backend" not in config.peers

                result = config.remove_peer("nonexistent")
                assert result is False

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
