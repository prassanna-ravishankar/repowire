from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

SCRIPT_PATH = Path(__file__).resolve().parents[1] / "scripts" / "agy_interop_smoke.py"
spec = importlib.util.spec_from_file_location("agy_interop_smoke", SCRIPT_PATH)
assert spec is not None and spec.loader is not None
agy_smoke = importlib.util.module_from_spec(spec)
sys.modules["agy_interop_smoke"] = agy_smoke
spec.loader.exec_module(agy_smoke)


class FakeResponse:
    def __init__(self, status_code: int, body: dict | list | str):
        self.status_code = status_code
        self._body = body
        self.text = body if isinstance(body, str) else ""

    def json(self):
        if isinstance(self._body, str):
            raise ValueError
        return self._body


class FakeClient:
    def __init__(self):
        self.calls: list[tuple[str, str, dict]] = []
        self.peers: dict[str, dict] = {}
        self.asks: dict[str, dict] = {}
        self.next_peer = 1
        self.register_body: dict | list | str | None = None
        self.ask_body: dict | list | str | None = None

    def request(self, method: str, url: str, **kwargs):
        self.calls.append((method, url, kwargs))
        path = url.removeprefix("http://daemon")
        params = kwargs.get("params") or {}
        body = kwargs.get("json") or {}

        if method == "GET" and path == "/health":
            return FakeResponse(200, {"ok": True})
        if method == "GET" and path == "/events":
            return FakeResponse(200, [])
        if method == "GET" and path == "/peers":
            peers = [
                p for p in self.peers.values()
                if p["backend"] == params.get("backend", p["backend"])
            ]
            return FakeResponse(200, {"peers": peers})
        if method == "POST" and path == "/peers":
            peer_id = f"p-{self.next_peer}"
            self.next_peer += 1
            if self.register_body is not None:
                return FakeResponse(200, self.register_body)
            peer = {
                "peer_id": peer_id,
                "display_name": body["name"],
                "backend": body["backend"],
                "metadata": body.get("metadata", {}),
            }
            self.peers[peer_id] = peer
            return FakeResponse(200, {"peer_id": peer_id, "display_name": body["name"]})
        if method == "DELETE" and path.startswith("/peers/"):
            peer_id = path.split("/", 2)[2]
            self.peers.pop(peer_id, None)
            return FakeResponse(200, {"ok": True})
        if method == "POST" and path == "/ask":
            if self.ask_body is not None:
                return FakeResponse(200, self.ask_body)
            self.asks["ask-1"] = {
                "correlation_id": "ask-1",
                "from_peer": body["from_peer"],
                "to_peer": body["to_peer"],
                "text": body["text"],
            }
            return FakeResponse(200, {"correlation_id": "ask-1"})
        if method == "GET" and path == "/asks/pending":
            peer_id = params["peer_id"]
            asks = [a for a in self.asks.values() if a["to_peer"] == peer_id]
            return FakeResponse(200, {"asks": asks})
        if method == "POST" and path == "/ack":
            self.asks.pop(body["correlation_id"], None)
            return FakeResponse(200, {"ok": True})
        raise AssertionError(f"unexpected request: {method} {path}")


def test_collect_mcp_evidence_is_explicitly_not_verified(monkeypatch):
    monkeypatch.setattr(agy_smoke.antigravity, "check_mcp_installed", lambda: False)

    evidence = agy_smoke.collect_mcp_evidence()

    assert evidence["status"] == "not_verified"
    assert evidence["check_mcp_installed"] is False
    assert "not verified" in evidence["note"]


def test_daemon_hook_evidence_ignores_cli_fallback_peers():
    client = FakeClient()
    client.peers["p-1"] = {
        "peer_id": "p-1",
        "display_name": "agy-cli",
        "backend": "antigravity",
        "metadata": {"repowire_cli_fallback": True},
    }

    evidence = agy_smoke.collect_daemon_evidence(client, "http://daemon", "default")

    assert evidence["hook_firing"]["status"] == "not_observed"
    assert evidence["hook_firing"]["non_cli_antigravity_peer_count"] == 0


def test_cli_fallback_smoke_registers_acks_and_deletes_temp_peers(monkeypatch):
    monkeypatch.setattr(
        "repowire.config.models.load_config",
        lambda: SimpleNamespace(daemon=SimpleNamespace(auth_token="token")),
    )
    client = FakeClient()

    result = agy_smoke.run_cli_fallback_smoke(client, "http://daemon", "default")

    assert result["status"] == "observed"
    assert result["saw_pending_before_ack"] is True
    assert result["cleared_after_ack"] is True
    assert [entry["name"] for entry in result["cleanup"]] == [
        "cleanup_ack",
        "delete_peer",
        "delete_peer",
    ]
    assert client.peers == {}
    assert client.asks == {}
    delete_calls = [c for c in client.calls if c[0] == "DELETE"]
    assert len(delete_calls) == 2


def test_cli_fallback_smoke_handles_malformed_register_success():
    client = FakeClient()
    client.register_body = "not-json"

    result = agy_smoke.run_cli_fallback_smoke(client, "http://daemon", "default")

    assert result["status"] == "failed"
    assert result["cleanup"] == []
    assert client.peers == {}


def test_cli_fallback_smoke_handles_malformed_ask_success():
    client = FakeClient()
    client.ask_body = "not-json"

    result = agy_smoke.run_cli_fallback_smoke(client, "http://daemon", "default")

    assert result["status"] == "failed"
    assert result["cleanup"][0]["name"] == "delete_peer"
    assert result["cleanup"][1]["name"] == "delete_peer"
    assert client.peers == {}
    assert client.asks == {}


def test_main_writes_json_report(tmp_path, monkeypatch):
    report = {"schema_version": 1, "evidence": {"cli_fallback_ask_ack": {"status": "not_run"}}}
    monkeypatch.setattr(agy_smoke, "build_report", MagicMock(return_value=report))
    out = tmp_path / "report.json"

    rc = agy_smoke.main(["--output", str(out)])

    assert rc == 0
    assert '"schema_version": 1' in out.read_text()
