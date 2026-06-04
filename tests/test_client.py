"""Tests for the public Repowire daemon client."""

from __future__ import annotations

import os
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest
from httpx import ASGITransport, AsyncClient

from repowire.client import AsyncRepowireClient, ClientPeer, ToolCall
from repowire.config.models import Config, DaemonConfig
from repowire.daemon.deps import cleanup_deps, get_config
from repowire.daemon.routes import asks, attachments, health, messages, peers
from repowire.daemon.routes import spawn as spawn_routes
from repowire.protocol.errors import DaemonHTTPError

from .conftest import async_client_for, make_daemon_app

ROUTERS = (
    health.router,
    peers.router,
    messages.router,
    asks.router,
    spawn_routes.router,
    attachments.router,
)


def _make_app(tmp_path: Path, config: Config | None = None):
    harness = make_daemon_app(tmp_path, ROUTERS, config=config)
    harness.message_router.send_ask = AsyncMock()
    harness.message_router.send_notification = AsyncMock()
    return harness.app


@pytest.fixture
async def client(tmp_path):
    app = _make_app(tmp_path)
    async with async_client_for(app) as http_client:
        yield AsyncRepowireClient(client=http_client)
    cleanup_deps()


async def test_health_and_spawn_config(client: AsyncRepowireClient):
    health_result = await client.health()
    assert health_result.status == "ok"

    spawn_config = await client.spawn_config()
    assert spawn_config.enabled is False
    assert spawn_config.commands == {}
    assert spawn_config.allowed_commands == []


async def test_spawn_config_includes_profiles(client: AsyncRepowireClient):
    cfg = get_config()
    cfg.daemon.spawn.commands[spawn_routes.AgentType.CODEX] = "codex"
    cfg.daemon.spawn.profiles = {
        spawn_routes.AgentType.CODEX: {
            "fast": {"args": ["--model", "gpt-5-mini"], "description": "Fast Codex"}
        }
    }
    cfg.daemon.spawn.allowed_paths = ["/"]

    spawn_config = await client.spawn_config()

    assert spawn_config.enabled is True
    assert spawn_config.profiles["codex"]["fast"]["args"] == ["--model", "gpt-5-mini"]


async def test_restart_peer_client_posts_payload(client: AsyncRepowireClient, tmp_path: Path):
    cfg = get_config()
    cfg.daemon.spawn.commands[spawn_routes.AgentType.CLAUDE_CODE] = "claude"
    cfg.daemon.spawn.allowed_paths = ["/"]
    registered = await client.register_peer(
        "proj",
        path=str(tmp_path),
        circle="default",
        pane_id="%77",
    )
    spawn_routes._SPAWNED_PANE_IDS.add("%77")
    try:
        with patch.object(spawn_routes, "kill_pane", return_value=True), \
            patch.object(
                spawn_routes,
                "spawn_peer",
                return_value=spawn_routes.SpawnResult(
                    display_name="proj",
                    tmux_session="default:proj",
                    pane_id="%88",
                ),
            ), \
            patch.object(spawn_routes, "post_spawn_warmup", new_callable=AsyncMock), \
            patch.object(
                spawn_routes,
                "resume_target",
                return_value=SimpleNamespace(
                    command="claude --resume test-session",
                    tmux_session="default:proj",
                ),
            ):
            result = await client.restart_peer(
                registered.peer_id,
                circle="default",
                from_peer="dashboard",
                message="reload context",
            )
    finally:
        spawn_routes._SPAWNED_PANE_IDS.discard("%77")
        spawn_routes._SPAWNED_PANE_IDS.discard("%88")

    assert result.status == "restarted"
    assert result.restarted is True
    assert result.peer_id == registered.peer_id
    assert result.resume_mode == "resumed"


async def test_resume_session_client_posts_payload(client: AsyncRepowireClient):
    client._request = AsyncMock(  # type: ignore[method-assign]
        return_value={
            "ok": True,
            "repowire_session_id": "rw-session-abc123",
            "session_status": "resumable",
            "status": "resume_available",
            "capability": "supported",
            "message": "Backend resume spawned for this runtime session.",
            "backend": "codex",
            "runtime_session_id": "codex-runtime-1",
            "action": "spawned",
            "spawned_display_name": "repo-codex",
            "tmux_session": "0:repo",
            "pane_id": "%99",
        }
    )

    result = await client.resume_session(
        "rw-session-abc123",
        dry_run=False,
        profile="fast",
        message="continue",
        from_peer="dashboard",
    )

    client._request.assert_awaited_once_with(  # type: ignore[attr-defined]
        "POST",
        "/sessions/rw-session-abc123/controls/resume",
        json={
            "dry_run": False,
            "profile": "fast",
            "message": "continue",
            "from_peer": "dashboard",
        },
    )
    assert result.action == "spawned"
    assert result.spawned_display_name == "repo-codex"


async def test_query_client_posts_compat_payload(client: AsyncRepowireClient):
    client._request = AsyncMock(  # type: ignore[method-assign]
        return_value={"text": "pong", "error": None, "status": None}
    )

    result = await client.query(
        "worker",
        "ping",
        from_peer="cli",
        timeout=7,
        bypass_circle=True,
        circle="team-a",
    )

    client._request.assert_awaited_once_with(  # type: ignore[attr-defined]
        "POST",
        "/query",
        json={
            "to_peer": "worker",
            "text": "ping",
            "bypass_circle": True,
            "from_peer": "cli",
            "timeout": 7,
            "circle": "team-a",
        },
    )
    assert result.text == "pong"


def test_client_peer_defaults_to_daemon_default_circle():
    peer = ClientPeer.model_validate(
        {
            "peer_id": "peer-1",
            "name": "alice",
            "display_name": "alice-claude-code",
            "status": "online",
        }
    )

    assert peer.circle == "default"


async def test_peer_lifecycle(client: AsyncRepowireClient):
    registered = await client.register_peer(
        "alice",
        path="/tmp/alice",
        circle="default",
        pane_id="%77",
        metadata={"project": "repowire"},
    )
    assert registered.display_name == "alice-claude-code"

    peers_list = await client.list_peers(status="online")
    assert [p.display_name for p in peers_list] == ["alice-claude-code"]
    assert peers_list[0].metadata["project"] == "repowire"

    by_name = await client.get_peer(registered.display_name)
    assert by_name.peer_id == registered.peer_id

    by_pane = await client.get_peer_by_pane("%77")
    assert by_pane.display_name == registered.display_name

    await client.set_description(registered.display_name, "testing client")
    described = await client.get_peer(registered.display_name)
    assert described.description == "testing client"

    await client.set_circle(registered.display_name, "next")
    moved = await client.get_peer(registered.display_name)
    assert moved.circle == "next"

    await client.unregister_peer(registered.peer_id)
    assert await client.list_peers() == []


async def test_ask_ack_and_pending(client: AsyncRepowireClient):
    alice = await client.register_peer("alice", path="/tmp/alice", circle="default")
    bob = await client.register_peer("bob", path="/tmp/bob", circle="default", pane_id="%88")

    opened = await client.ask(bob.display_name, "ship it?", from_peer=alice.display_name)
    assert opened.correlation_id.startswith("ask-")

    pending = await client.pending_asks(pane_id="%88")
    assert len(pending) == 1
    assert pending[0].correlation_id == opened.correlation_id
    assert pending[0].from_peer == alice.display_name
    assert pending[0].to_peer == bob.display_name
    assert pending[0].direction == "inbound"

    outbound = await client.pending_asks(peer_id=alice.peer_id, direction="outbound")
    assert len(outbound) == 1
    assert outbound[0].correlation_id == opened.correlation_id
    assert outbound[0].direction == "outbound"

    both = await client.pending_asks(peer_id=alice.peer_id, direction="both")
    assert [ask.direction for ask in both] == ["outbound"]

    await client.ack(opened.correlation_id, from_peer=bob.display_name)
    assert await client.pending_asks(peer_id=bob.peer_id) == []


async def test_ask_notify_ack_accept_attachments(client: AsyncRepowireClient):
    alice = await client.register_peer("alice", path="/tmp/alice", circle="default")
    bob = await client.register_peer("bob", path="/tmp/bob", circle="default")
    attachment = {
        "id": "att123",
        "path": "/tmp/att123.png",
        "filename": "diagram.png",
        "content_type": "image/png",
    }

    opened = await client.ask(
        bob.display_name,
        "see image",
        from_peer=alice.display_name,
        attachments=[attachment],
    )
    await client.ack(
        opened.correlation_id,
        from_peer=bob.display_name,
        message="reply",
        attachments=[attachment],
    )
    await client.notify(
        bob.display_name,
        "heads up",
        from_peer=alice.display_name,
        attachments=[attachment],
    )

    events = await client.events()
    assert events[0].attachments[0]["filename"] == "diagram.png"
    assert events[-1].attachments[0]["id"] == "att123"


async def test_notify_broadcast_events_and_chat_ingest(client: AsyncRepowireClient):
    alice = await client.register_peer("alice", path="/tmp/alice", circle="default")
    bob = await client.register_peer("bob", path="/tmp/bob", circle="default")

    await client.notify(bob.display_name, "hello", from_peer=alice.display_name)
    broadcast = await client.broadcast("all hands", from_peer=alice.display_name)

    assert broadcast.ok is True
    assert broadcast.sent_to == []

    await client.ingest_chat_turn(
        alice.display_name,
        "assistant",
        "done",
        peer_id=alice.peer_id,
        tool_calls=[ToolCall(name="list_peers", input="{}")],
    )
    events = await client.events()
    assert [event.type for event in events] == ["notification", "broadcast", "chat_turn"]
    assert events[-1].tool_calls[0]["name"] == "list_peers"


async def test_orchestrator_status(client: AsyncRepowireClient):
    status = await client.orchestrator_status("default")
    assert status.circle == "default"
    assert status.present is False
    assert status.stale_after_seconds > 0


async def test_upload_and_download_attachment(client: AsyncRepowireClient, tmp_path):
    attachment_dir = tmp_path / "attachments"
    with patch.object(attachments, "ATTACHMENTS_DIR", attachment_dir):
        uploaded = await client.upload_attachment(
            b"hello attachment",
            filename="note.txt",
            content_type="text/plain",
        )
        assert uploaded.filename == "note.txt"
        assert uploaded.size == len(b"hello attachment")

        downloaded = await client.download_attachment(uploaded.id)
        assert downloaded == b"hello attachment"


async def test_attachment_quota_rejects_when_dir_full(
    client: AsyncRepowireClient, tmp_path
):
    """When the attachments dir already exceeds MAX_DIR_SIZE, upload returns 507."""
    attachment_dir = tmp_path / "attachments"
    attachment_dir.mkdir(parents=True)
    # Pre-fill the dir with one file at the cap so a fresh upload tips it over.
    fat = attachment_dir / "pre-existing.bin"
    fat.write_bytes(b"\x00" * (attachments.MAX_DIR_SIZE))

    with (
        patch.object(attachments, "ATTACHMENTS_DIR", attachment_dir),
        patch.object(attachments, "MAX_AGE_HOURS", 999_999),  # block TTL cleanup
    ):
        with pytest.raises(DaemonHTTPError) as exc_info:
            await client.upload_attachment(
                b"new content", filename="new.txt", content_type="text/plain"
            )
        assert exc_info.value.status == 507
        assert "full" in str(exc_info.value).lower() or "cap" in str(exc_info.value).lower()


async def test_attachment_quota_allows_after_ttl_sweep(
    client: AsyncRepowireClient, tmp_path
):
    """If pre-existing files are expired, the upload handler sweeps them and the upload succeeds."""
    attachment_dir = tmp_path / "attachments"
    attachment_dir.mkdir(parents=True)
    stale = attachment_dir / "stale.bin"
    stale.write_bytes(b"\x00" * attachments.MAX_DIR_SIZE)
    # Force the file's mtime to look expired.
    old = time.time() - (attachments.MAX_AGE_HOURS + 1) * 3600
    os.utime(stale, (old, old))

    with patch.object(attachments, "ATTACHMENTS_DIR", attachment_dir):
        uploaded = await client.upload_attachment(
            b"fresh upload", filename="fresh.txt", content_type="text/plain"
        )
        assert uploaded.size == len(b"fresh upload")
    assert not stale.exists()


async def test_owned_client_does_not_duplicate_auth_headers():
    client = AsyncRepowireClient(auth_token="secret")
    try:
        assert "authorization" not in client._client.headers
    finally:
        await client.aclose()


async def test_spawn_omits_default_circle_for_daemon_default():
    client = AsyncRepowireClient(client=AsyncMock())
    client._request = AsyncMock(  # type: ignore[method-assign]
        return_value={"ok": True, "display_name": "proj-codex", "tmux_session": "default"}
    )

    result = await client.spawn("/tmp/proj", backend="codex", message="warm up")

    assert result.display_name == "proj-codex"
    client._request.assert_awaited_once_with(
        "POST",
        "/spawn",
        json={"path": "/tmp/proj", "backend": "codex", "message": "warm up"},
    )


async def test_spawn_posts_profile():
    client = AsyncRepowireClient(client=AsyncMock())
    client._request = AsyncMock(  # type: ignore[method-assign]
        return_value={"ok": True, "display_name": "proj-codex", "tmux_session": "default"}
    )

    await client.spawn("/tmp/proj", backend="codex", profile="fast")

    client._request.assert_awaited_once_with(
        "POST",
        "/spawn",
        json={"path": "/tmp/proj", "backend": "codex", "profile": "fast"},
    )


async def test_auth_token_is_sent(tmp_path):
    app = _make_app(tmp_path, Config(daemon=DaemonConfig(auth_token="secret")))
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as http_client:
        authed = AsyncRepowireClient(client=http_client, auth_token="secret")
        assert (await authed.health()).status == "ok"

        bad = AsyncRepowireClient(client=http_client, auth_token="wrong")
        with pytest.raises(DaemonHTTPError) as exc_info:
            await bad.list_peers()
        assert exc_info.value.status == 401
    cleanup_deps()
