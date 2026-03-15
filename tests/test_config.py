import tempfile
from pathlib import Path
from unittest.mock import patch

from repowire.config.models import Config, PeerConfig, load_config


class TestConfig:
    def test_default_config(self):
        config = Config()

        assert config.relay.enabled is False
        assert config.relay.url == "wss://repowire.io"
        assert len(config.peers) == 0

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
