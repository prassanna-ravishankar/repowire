"""Tests for repowire.peer_describe (pure-function module + resolver)."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import httpx
import pytest

from repowire.peer_describe import (
    ActivityEvent,
    PendingAsk,
    ResolveAmbiguous,
    ResolveNotFound,
    ambiguity_message,
    build_snapshot,
    fetch_all_peers,
    fetch_pending_asks,
    fetch_recent_activity,
    humanize_last_seen,
    resolve_peer,
)


def _peer(
    peer_id: str,
    name: str,
    circle: str = "global",
    status: str = "online",
    **extra: object,
) -> dict:
    p: dict = {
        "peer_id": peer_id,
        "name": name,
        "display_name": name,
        "circle": circle,
        "status": status,
        "backend": "claude-code",
        "role": "agent",
        "path": "/tmp/x",
        "machine": "host",
        "last_seen": "2026-05-15T14:55:00+00:00",
        "turn_state": "idle",
        "description": "",
        "metadata": {},
    }
    p.update(extra)
    return p


def _client(handler):
    """Build an httpx.Client backed by a MockTransport."""
    transport = httpx.MockTransport(handler)
    return httpx.Client(transport=transport, base_url="http://test")


# ---------------------------------------------------------------------------
# resolve_peer
# ---------------------------------------------------------------------------


class TestResolvePeer:
    def test_single_match_by_name(self):
        peers = [_peer("p-1", "alice")]
        r = resolve_peer(peers, "alice")
        assert isinstance(r, dict)
        assert r["peer_id"] == "p-1"

    def test_single_match_by_peer_id(self):
        peers = [_peer("p-1", "alice")]
        r = resolve_peer(peers, "p-1")
        assert isinstance(r, dict)
        assert r["peer_id"] == "p-1"

    def test_not_found(self):
        peers = [_peer("p-1", "alice"), _peer("p-2", "bob")]
        r = resolve_peer(peers, "carol")
        assert isinstance(r, ResolveNotFound)
        assert r.identifier == "carol"
        assert "alice" in r.known_names
        assert "bob" in r.known_names

    def test_ambiguous_across_circles(self):
        peers = [
            _peer("p-1", "alice", circle="5"),
            _peer("p-2", "alice", circle="default"),
        ]
        r = resolve_peer(peers, "alice")
        assert isinstance(r, ResolveAmbiguous)
        assert sorted(r.circles) == ["5", "default"]

    def test_ambiguous_resolved_by_circle(self):
        peers = [
            _peer("p-1", "alice", circle="5"),
            _peer("p-2", "alice", circle="default"),
        ]
        r = resolve_peer(peers, "alice", circle="5")
        assert isinstance(r, dict)
        assert r["peer_id"] == "p-1"

    def test_circle_filter_yields_not_found(self):
        peers = [_peer("p-1", "alice", circle="5")]
        r = resolve_peer(peers, "alice", circle="default")
        assert isinstance(r, ResolveNotFound)

    def test_peer_id_wins_over_display_name_match(self):
        peers = [
            _peer("p-1", "alice", circle="5"),
            _peer("alice", "carol", circle="default"),  # peer_id == "alice"
        ]
        r = resolve_peer(peers, "alice")
        assert isinstance(r, dict)
        assert r["peer_id"] == "alice"

    def test_ambiguity_message_format(self):
        amb = ResolveAmbiguous(identifier="alice", circles=["5", "default"])
        msg = ambiguity_message(amb)
        assert "Ambiguous peer name 'alice'" in msg
        assert "['5', 'default']" in msg
        assert "--circle=" in msg


# ---------------------------------------------------------------------------
# fetch_all_peers
# ---------------------------------------------------------------------------


class TestFetchAllPeers:
    def test_returns_list(self):
        def handler(request):
            assert request.url.path == "/peers"
            return httpx.Response(200, json={"peers": [_peer("p-1", "alice")]})

        with _client(handler) as client:
            peers = fetch_all_peers(client, "http://test")
        assert len(peers) == 1
        assert peers[0]["display_name"] == "alice"

    def test_empty_peers(self):
        def handler(_request):
            return httpx.Response(200, json={"peers": []})

        with _client(handler) as client:
            peers = fetch_all_peers(client, "http://test")
        assert peers == []

    def test_http_error_raises(self):
        def handler(_request):
            return httpx.Response(500, json={"detail": "boom"})

        with _client(handler) as client, pytest.raises(httpx.HTTPError):
            fetch_all_peers(client, "http://test")


# ---------------------------------------------------------------------------
# fetch_pending_asks
# ---------------------------------------------------------------------------


class TestFetchPendingAsks:
    def test_returns_both_directions(self):
        def handler(request):
            assert request.url.path == "/asks/pending"
            assert request.url.params["peer_id"] == "p-1"
            assert request.url.params["direction"] == "both"
            return httpx.Response(
                200,
                json={
                    "asks": [
                        {
                            "correlation_id": "ask-a",
                            "from_peer": "alice",
                            "to_peer": "bob",
                            "text": "hello",
                            "created_at": "2026-05-15T14:53:00+00:00",
                            "direction": "inbound",
                        },
                        {
                            "correlation_id": "ask-b",
                            "from_peer": "bob",
                            "to_peer": "carol",
                            "text": "out",
                            "created_at": "2026-05-15T14:54:00+00:00",
                            "direction": "outbound",
                        },
                    ],
                },
            )

        with _client(handler) as client:
            asks = fetch_pending_asks(client, "http://test", "p-1")
        assert len(asks) == 2
        assert asks[0].direction == "inbound"
        assert asks[1].direction == "outbound"

    def test_404_returns_empty(self):
        def handler(_request):
            return httpx.Response(404, json={"detail": "no peer"})

        with _client(handler) as client:
            asks = fetch_pending_asks(client, "http://test", "p-missing")
        assert asks == []


# ---------------------------------------------------------------------------
# fetch_recent_activity
# ---------------------------------------------------------------------------


class TestFetchRecentActivity:
    def test_filters_by_peer_id_or_name(self):
        def handler(_request):
            return httpx.Response(
                200,
                json=[
                    # Inbound (notification to p-1)
                    {
                        "id": "1",
                        "type": "notification",
                        "timestamp": "2026-05-15T14:50:00+00:00",
                        "from": "alice",
                        "to": "bob",
                        "from_peer_id": "p-a",
                        "to_peer_id": "p-1",
                        "text": "hi bob",
                    },
                    # Outbound (p-1 -> someone)
                    {
                        "id": "2",
                        "type": "notification",
                        "timestamp": "2026-05-15T14:51:00+00:00",
                        "from": "bob",
                        "to": "carol",
                        "from_peer_id": "p-1",
                        "to_peer_id": "p-c",
                        "text": "bye carol",
                    },
                    # Irrelevant
                    {
                        "id": "3",
                        "type": "notification",
                        "timestamp": "2026-05-15T14:52:00+00:00",
                        "from": "alice",
                        "to": "carol",
                        "from_peer_id": "p-a",
                        "to_peer_id": "p-c",
                        "text": "nope",
                    },
                ],
            )

        with _client(handler) as client:
            events = fetch_recent_activity(client, "http://test", "p-1", "bob")

        assert len(events) == 2
        # Newest first
        assert events[0].timestamp == "2026-05-15T14:51:00+00:00"
        assert events[0].direction == "outbound"
        assert events[0].counterparty == "carol"
        assert events[1].direction == "inbound"

    def test_handles_non_list_response(self):
        def handler(_request):
            return httpx.Response(200, json={"oops": "not a list"})

        with _client(handler) as client:
            events = fetch_recent_activity(client, "http://test", "p-1", "bob")
        assert events == []

    def test_limit_caps_results(self):
        def handler(_request):
            return httpx.Response(
                200,
                json=[
                    {
                        "id": str(i),
                        "type": "notification",
                        "timestamp": f"2026-05-15T14:{50 + i:02d}:00+00:00",
                        "from": "alice",
                        "to": "bob",
                        "from_peer_id": "p-a",
                        "to_peer_id": "p-1",
                        "text": f"msg {i}",
                    }
                    for i in range(10)
                ],
            )

        with _client(handler) as client:
            events = fetch_recent_activity(client, "http://test", "p-1", "bob", limit=3)
        assert len(events) == 3


# ---------------------------------------------------------------------------
# build_snapshot
# ---------------------------------------------------------------------------


class TestBuildSnapshot:
    def test_online_peer_full_snapshot(self):
        peer = _peer("p-1", "bob")

        def handler(request):
            path = request.url.path
            if path == "/asks/pending":
                return httpx.Response(
                    200,
                    json={
                        "asks": [
                            {
                                "correlation_id": "ask-in",
                                "from_peer": "alice",
                                "to_peer": "bob",
                                "text": "?",
                                "created_at": "2026-05-15T14:53:00+00:00",
                                "direction": "inbound",
                            },
                        ],
                    },
                )
            if path == "/events":
                return httpx.Response(
                    200,
                    json=[
                        {
                            "id": "1",
                            "type": "notification",
                            "timestamp": "2026-05-15T14:54:00+00:00",
                            "from": "alice",
                            "to": "bob",
                            "from_peer_id": "p-a",
                            "to_peer_id": "p-1",
                            "text": "hi",
                        },
                    ],
                )
            return httpx.Response(404)

        with _client(handler) as client:
            snap = build_snapshot(client, "http://test", peer)

        assert snap.peer["peer_id"] == "p-1"
        assert len(snap.inbound_asks) == 1
        assert snap.inbound_asks[0].correlation_id == "ask-in"
        assert snap.outbound_asks == []
        assert len(snap.recent_activity) == 1

    def test_offline_peer_still_renders_with_no_asks(self):
        peer = _peer("p-1", "bob", status="offline")

        def handler(request):
            if request.url.path == "/asks/pending":
                # Daemon may have evicted; return 404 to mimic forget_peer cleanup
                return httpx.Response(404, json={"detail": "no peer"})
            if request.url.path == "/events":
                return httpx.Response(200, json=[])
            return httpx.Response(404)

        with _client(handler) as client:
            snap = build_snapshot(client, "http://test", peer)

        assert snap.peer["status"] == "offline"
        assert snap.inbound_asks == []
        assert snap.outbound_asks == []
        assert snap.recent_activity == []


# ---------------------------------------------------------------------------
# humanize_last_seen
# ---------------------------------------------------------------------------


class TestHumanizeLastSeen:
    def test_empty_returns_never(self):
        assert humanize_last_seen(None) == "never"
        assert humanize_last_seen("") == "never"

    def test_seconds_ago(self):
        now = datetime(2026, 5, 15, 15, 0, 0, tzinfo=timezone.utc)
        iso = (now - timedelta(seconds=30)).isoformat()
        s = humanize_last_seen(iso, now=now)
        assert "30s ago" in s

    def test_minutes_ago(self):
        now = datetime(2026, 5, 15, 15, 0, 0, tzinfo=timezone.utc)
        iso = (now - timedelta(minutes=5)).isoformat()
        s = humanize_last_seen(iso, now=now)
        assert "5m ago" in s

    def test_hours_ago(self):
        now = datetime(2026, 5, 15, 15, 0, 0, tzinfo=timezone.utc)
        iso = (now - timedelta(hours=3)).isoformat()
        s = humanize_last_seen(iso, now=now)
        assert "3h ago" in s

    def test_days_ago(self):
        now = datetime(2026, 5, 15, 15, 0, 0, tzinfo=timezone.utc)
        iso = (now - timedelta(days=2)).isoformat()
        s = humanize_last_seen(iso, now=now)
        assert "2d ago" in s

    def test_malformed_returns_input(self):
        assert humanize_last_seen("garbage") == "garbage"


# ---------------------------------------------------------------------------
# Dataclass smoke checks
# ---------------------------------------------------------------------------


def test_dataclass_construct_smoke():
    a = PendingAsk(
        correlation_id="ask-1",
        direction="inbound",
        from_peer="alice",
        to_peer="bob",
        text="?",
        created_at="now",
    )
    assert a.direction == "inbound"

    e = ActivityEvent(
        event_type="notification",
        timestamp="now",
        direction="outbound",
        counterparty="alice",
        text="hi",
    )
    assert e.event_type == "notification"


# ---------------------------------------------------------------------------
# Git status parsing (compute_git_status integration tests use real repos)
# ---------------------------------------------------------------------------


from repowire.peer_describe import _parse_git_status_porcelain, compute_git_status  # noqa: E402


def test_parse_clean_no_upstream():
    out = "# branch.oid abc123\n# branch.head main\n"
    assert _parse_git_status_porcelain(out) == {"ahead": 0, "behind": 0, "dirty": 0, "staged": 0}


def test_parse_clean_with_upstream():
    out = (
        "# branch.oid abc123\n"
        "# branch.head main\n"
        "# branch.upstream origin/main\n"
        "# branch.ab +0 -0\n"
    )
    assert _parse_git_status_porcelain(out) == {"ahead": 0, "behind": 0, "dirty": 0, "staged": 0}


def test_parse_ahead_only():
    out = "# branch.ab +3 -0\n"
    assert _parse_git_status_porcelain(out)["ahead"] == 3
    assert _parse_git_status_porcelain(out)["behind"] == 0


def test_parse_behind_only():
    out = "# branch.ab +0 -2\n"
    res = _parse_git_status_porcelain(out)
    assert res["ahead"] == 0
    assert res["behind"] == 2


def test_parse_ahead_and_behind():
    out = "# branch.ab +1 -2\n"
    res = _parse_git_status_porcelain(out)
    assert res["ahead"] == 1 and res["behind"] == 2


def test_parse_dirty_unstaged():
    out = "1 .M N... 100644 100644 100644 aaa bbb file.txt\n"
    res = _parse_git_status_porcelain(out)
    assert res["dirty"] == 1 and res["staged"] == 0


def test_parse_staged_only():
    out = "1 M. N... 100644 100644 100644 aaa bbb file.txt\n"
    res = _parse_git_status_porcelain(out)
    assert res["staged"] == 1 and res["dirty"] == 0


def test_parse_staged_and_unstaged_same_file():
    out = "1 MM N... 100644 100644 100644 aaa bbb file.txt\n"
    res = _parse_git_status_porcelain(out)
    assert res["staged"] == 1 and res["dirty"] == 1


def test_parse_untracked_counts_as_dirty():
    out = "? newfile.txt\n? other.py\n"
    res = _parse_git_status_porcelain(out)
    assert res["dirty"] == 2 and res["staged"] == 0


def test_parse_unmerged_counts_both():
    out = "u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.txt\n"
    res = _parse_git_status_porcelain(out)
    assert res["staged"] == 1 and res["dirty"] == 1


def test_parse_combined():
    out = (
        "# branch.ab +1 -2\n"
        "1 M. N... 100644 100644 100644 aaa bbb staged.txt\n"
        "1 .M N... 100644 100644 100644 aaa bbb dirty.txt\n"
        "? untracked.txt\n"
    )
    assert _parse_git_status_porcelain(out) == {"ahead": 1, "behind": 2, "dirty": 2, "staged": 1}


def test_compute_git_status_non_git_dir(tmp_path):
    assert compute_git_status(str(tmp_path)) is None


def _git(cwd, *args):
    import subprocess

    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


def test_compute_git_status_clean_repo(tmp_path):
    _git(tmp_path, "init", "-q", "-b", "main")
    _git(tmp_path, "config", "user.email", "t@t")
    _git(tmp_path, "config", "user.name", "t")
    _git(tmp_path, "commit", "--allow-empty", "-m", "init")
    res = compute_git_status(str(tmp_path))
    assert res == {"ahead": 0, "behind": 0, "dirty": 0, "staged": 0}


def test_compute_git_status_dirty_and_untracked(tmp_path):
    _git(tmp_path, "init", "-q", "-b", "main")
    _git(tmp_path, "config", "user.email", "t@t")
    _git(tmp_path, "config", "user.name", "t")
    (tmp_path / "a.txt").write_text("a")
    _git(tmp_path, "add", "a.txt")
    _git(tmp_path, "commit", "-m", "init")
    (tmp_path / "a.txt").write_text("changed")  # unstaged
    (tmp_path / "b.txt").write_text("new")  # untracked
    (tmp_path / "c.txt").write_text("staged")
    _git(tmp_path, "add", "c.txt")  # staged
    res = compute_git_status(str(tmp_path))
    assert res is not None
    assert res["dirty"] == 2  # a.txt modified + b.txt untracked
    assert res["staged"] == 1


def test_compute_git_status_ahead(tmp_path):
    import subprocess

    # Create a bare upstream, clone, commit ahead.
    upstream = tmp_path / "upstream.git"
    work = tmp_path / "work"
    subprocess.run(
        ["git", "init", "-q", "--bare", "-b", "main", str(upstream)],
        check=True,
        capture_output=True,
    )
    subprocess.run(
        ["git", "clone", "-q", str(upstream), str(work)], check=True, capture_output=True
    )
    _git(work, "config", "user.email", "t@t")
    _git(work, "config", "user.name", "t")
    _git(work, "commit", "--allow-empty", "-m", "first")
    _git(work, "push", "-q", "origin", "main")
    _git(work, "commit", "--allow-empty", "-m", "ahead1")
    _git(work, "commit", "--allow-empty", "-m", "ahead2")
    res = compute_git_status(str(work))
    assert res is not None
    assert res["ahead"] == 2 and res["behind"] == 0


def test_compute_git_status_behind(tmp_path):
    import subprocess

    upstream = tmp_path / "upstream.git"
    work_a = tmp_path / "a"
    work_b = tmp_path / "b"
    subprocess.run(
        ["git", "init", "-q", "--bare", "-b", "main", str(upstream)],
        check=True,
        capture_output=True,
    )
    subprocess.run(
        ["git", "clone", "-q", str(upstream), str(work_a)], check=True, capture_output=True
    )
    _git(work_a, "config", "user.email", "t@t")
    _git(work_a, "config", "user.name", "t")
    _git(work_a, "commit", "--allow-empty", "-m", "first")
    _git(work_a, "push", "-q", "origin", "main")
    subprocess.run(
        ["git", "clone", "-q", str(upstream), str(work_b)], check=True, capture_output=True
    )
    _git(work_b, "config", "user.email", "t@t")
    _git(work_b, "config", "user.name", "t")
    # Now push more from A so B is behind.
    _git(work_a, "commit", "--allow-empty", "-m", "second")
    _git(work_a, "push", "-q", "origin", "main")
    _git(work_b, "fetch", "-q")
    res = compute_git_status(str(work_b))
    assert res is not None
    assert res["behind"] == 1 and res["ahead"] == 0


def test_compute_git_status_ahead_and_behind(tmp_path):
    import subprocess

    upstream = tmp_path / "upstream.git"
    work_a = tmp_path / "a"
    work_b = tmp_path / "b"
    subprocess.run(
        ["git", "init", "-q", "--bare", "-b", "main", str(upstream)],
        check=True,
        capture_output=True,
    )
    subprocess.run(
        ["git", "clone", "-q", str(upstream), str(work_a)], check=True, capture_output=True
    )
    _git(work_a, "config", "user.email", "t@t")
    _git(work_a, "config", "user.name", "t")
    _git(work_a, "commit", "--allow-empty", "-m", "first")
    _git(work_a, "push", "-q", "origin", "main")
    subprocess.run(
        ["git", "clone", "-q", str(upstream), str(work_b)], check=True, capture_output=True
    )
    _git(work_b, "config", "user.email", "t@t")
    _git(work_b, "config", "user.name", "t")
    # A pushes ahead by 1.
    _git(work_a, "commit", "--allow-empty", "-m", "second")
    _git(work_a, "push", "-q", "origin", "main")
    # B makes a local commit (ahead 1) then fetches (behind 1 from A's push).
    _git(work_b, "commit", "--allow-empty", "-m", "local")
    _git(work_b, "fetch", "-q")
    res = compute_git_status(str(work_b))
    assert res is not None
    assert res["ahead"] == 1 and res["behind"] == 1
