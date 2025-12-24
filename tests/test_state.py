import json
import pytest
from pathlib import Path

from repowire.mesh.state import SharedState


class TestSharedState:
    @pytest.fixture
    def temp_state_file(self, tmp_path):
        return tmp_path / "state.json"

    @pytest.fixture
    def state(self, temp_state_file):
        return SharedState(persist_path=temp_state_file)

    @pytest.mark.asyncio
    async def test_write_and_read(self, state):
        await state.write("key1", "value1")
        result = await state.read("key1")
        assert result == "value1"

    @pytest.mark.asyncio
    async def test_read_nonexistent_key(self, state):
        result = await state.read("nonexistent")
        assert result is None

    @pytest.mark.asyncio
    async def test_read_all(self, state):
        await state.write("key1", "value1")
        await state.write("key2", "value2")
        result = await state.read()
        assert result == {"key1": "value1", "key2": "value2"}

    @pytest.mark.asyncio
    async def test_read_all_empty(self, state):
        result = await state.read()
        assert result == {}

    @pytest.mark.asyncio
    async def test_delete(self, state):
        await state.write("key1", "value1")
        result = await state.delete("key1")
        assert result is True
        assert await state.read("key1") is None

    @pytest.mark.asyncio
    async def test_delete_nonexistent(self, state):
        result = await state.delete("nonexistent")
        assert result is False

    @pytest.mark.asyncio
    async def test_clear(self, state):
        await state.write("key1", "value1")
        await state.write("key2", "value2")
        await state.clear()
        result = await state.read()
        assert result == {}

    @pytest.mark.asyncio
    async def test_persistence(self, temp_state_file):
        state1 = SharedState(persist_path=temp_state_file)
        await state1.write("persistent_key", "persistent_value")

        state2 = SharedState(persist_path=temp_state_file)
        result = await state2.read("persistent_key")
        assert result == "persistent_value"

    @pytest.mark.asyncio
    async def test_write_complex_value(self, state):
        complex_value = {"nested": {"key": "value"}, "list": [1, 2, 3]}
        await state.write("complex", complex_value)
        result = await state.read("complex")
        assert result == complex_value

    @pytest.mark.asyncio
    async def test_overwrite_value(self, state):
        await state.write("key", "value1")
        await state.write("key", "value2")
        result = await state.read("key")
        assert result == "value2"

    @pytest.mark.asyncio
    async def test_file_created(self, state, temp_state_file):
        await state.write("key", "value")
        assert temp_state_file.exists()

    @pytest.mark.asyncio
    async def test_file_content(self, state, temp_state_file):
        await state.write("key", "value")
        content = json.loads(temp_state_file.read_text())
        assert content == {"key": "value"}

    @pytest.mark.asyncio
    async def test_parent_directory_created(self, tmp_path):
        nested_path = tmp_path / "nested" / "dir" / "state.json"
        state = SharedState(persist_path=nested_path)
        await state.write("key", "value")
        assert nested_path.exists()
