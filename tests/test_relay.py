import pytest
import tempfile
from pathlib import Path
from unittest.mock import patch
from datetime import datetime

from repowire.relay.auth import generate_api_key, validate_api_key, APIKey


class TestRelayAuth:
    def test_generate_api_key(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            keys_file = Path(tmpdir) / "api_keys.json"
            with patch("repowire.relay.auth.API_KEYS_PATH", keys_file):
                api_key = generate_api_key("user1", "test-key")

                assert api_key.key.startswith("rw_")
                assert api_key.user_id == "user1"
                assert api_key.name == "test-key"
                assert keys_file.exists()

    def test_validate_api_key(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            keys_file = Path(tmpdir) / "api_keys.json"
            with patch("repowire.relay.auth.API_KEYS_PATH", keys_file):
                generated = generate_api_key("user1", "test")

                validated = validate_api_key(generated.key)

                assert validated is not None
                assert validated.user_id == "user1"
                assert validated.key == generated.key

    def test_validate_invalid_key(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            keys_file = Path(tmpdir) / "api_keys.json"
            with patch("repowire.relay.auth.API_KEYS_PATH", keys_file):
                result = validate_api_key("rw_invalid_key")
                assert result is None

    def test_api_key_model(self):
        key = APIKey(
            key="rw_test123",
            user_id="user1",
            name="test",
            created_at=datetime.utcnow(),
        )

        assert key.key == "rw_test123"
        assert key.last_used is None
