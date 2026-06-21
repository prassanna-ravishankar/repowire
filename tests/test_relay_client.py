"""Tests for the daemon-side RelayClient liveness + health telemetry.

Guards the failure mode where the reconnect loop silently died (no logs for
hours) while /health still reported relay as up. See repowire-blr.
"""

from __future__ import annotations

import asyncio

from repowire.config.relay import RelayConfig
from repowire.daemon import relay_client as relay_client_mod
from repowire.daemon.relay_client import (
    _PING_INTERVAL,
    _PING_TIMEOUT,
    RelayClient,
)


def _client() -> RelayClient:
    cfg = RelayConfig(enabled=True, api_key="rw_testtesttesttesttesttest", url="wss://relay.test")
    return RelayClient(config=cfg, daemon_id="daemon-test")


class TestRelayStatus:
    def test_status_disconnected_by_default(self):
        client = _client()
        st = client.status()
        assert st["connected"] is False
        assert st["running"] is False
        assert st["url"] == "wss://relay.test"
        assert st["daemon_id"] == "daemon-test"
        assert st["last_connected_at"] is None
        assert st["last_error"] is None

    def test_status_reflects_last_error(self):
        client = _client()
        client._last_error = "ConnectionClosedError: no close frame"
        st = client.status()
        assert st["last_error"].startswith("ConnectionClosedError")


class TestEnsureRunning:
    async def test_ensure_running_relaunches_dead_loop(self, monkeypatch):
        client = _client()

        started = asyncio.Event()

        async def fake_loop():
            started.set()
            # Stay alive until cancelled so the task is "running".
            await asyncio.Event().wait()

        monkeypatch.setattr(client, "_run_loop", fake_loop)

        # No task yet -> ensure_running starts one.
        relaunched = await client.ensure_running()
        assert relaunched is True
        await asyncio.wait_for(started.wait(), timeout=1)
        assert client.status()["running"] is True

        # Already running -> no-op (no duplicate task).
        first_task = client._task
        assert await client.ensure_running() is False
        assert client._task is first_task

        await client.stop()

    async def test_ensure_running_noop_when_disabled(self):
        cfg = RelayConfig(enabled=False, api_key="rw_x", url="wss://relay.test")
        client = RelayClient(config=cfg)
        assert await client.ensure_running() is False
        assert client._task is None

    async def test_ensure_running_noop_when_stopping(self):
        client = _client()
        client._stopping = True
        assert await client.ensure_running() is False


class TestConnectArgs:
    async def test_connect_uses_explicit_keepalive_ping(self, monkeypatch):
        """The half-open-detection fix: ping_interval/timeout are passed explicitly."""
        captured: dict = {}

        class FakeWS:
            close_code = None

            async def __aenter__(self):
                return self

            async def __aexit__(self, *a):
                return False

        def fake_connect(url, **kwargs):
            captured.update(kwargs)
            # Stop the loop after one connect attempt.
            client._stopping = True
            return FakeWS()

        async def fake_listen(ws):
            return None

        monkeypatch.setattr(relay_client_mod.websockets, "connect", fake_connect)

        client = _client()
        monkeypatch.setattr(client, "_listen", fake_listen)
        await client._run_loop()

        assert captured["ping_interval"] == _PING_INTERVAL
        assert captured["ping_timeout"] == _PING_TIMEOUT

    async def test_loop_records_last_error_on_failure(self, monkeypatch):
        attempts = {"n": 0}

        def fake_connect(url, **kwargs):
            attempts["n"] += 1
            client._stopping = True  # one shot
            raise RuntimeError("boom relay")

        monkeypatch.setattr(relay_client_mod.websockets, "connect", fake_connect)
        # Don't actually sleep on backoff.
        monkeypatch.setattr(relay_client_mod.asyncio, "sleep", _noop_sleep)

        client = _client()
        await client._run_loop()

        assert attempts["n"] == 1
        st = client.status()
        assert st["last_error"] == "RuntimeError: boom relay"
        assert st["last_error_at"] is not None


async def _noop_sleep(_seconds):
    return None
