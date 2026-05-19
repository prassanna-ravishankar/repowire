"""Tests for the per-turn transcript-tailing chat_turn_delta streamer."""

from __future__ import annotations

import json
import os
import threading
import time
from unittest.mock import patch

import pytest

from repowire.hooks import chat_delta_streamer as streamer


def _claude_assistant_line(text: str | None = None, tool_use: dict | None = None) -> str:
    content: list[dict] = []
    if text is not None:
        content.append({"type": "text", "text": text})
    if tool_use is not None:
        content.append({"type": "tool_use", **tool_use})
    return json.dumps({
        "type": "assistant",
        "uuid": "uuid-1",
        "parentUuid": "uuid-0",
        "message": {"content": content},
    }) + "\n"


@pytest.fixture(autouse=True)
def _fast_polling(monkeypatch):
    """Tighten poll interval so tests don't drag."""
    monkeypatch.setattr(streamer, "POLL_INTERVAL_S", 0.01)
    monkeypatch.setattr(streamer, "IDLE_EXIT_AFTER_S", 0.5)


def test_streamer_emits_text_block_delta(tmp_path):
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")  # exists but empty at start
    posts: list[tuple[str, dict]] = []

    def fake_post(path: str, payload: dict, **_kw):
        posts.append((path, payload))
        return {}

    def write_after_delay():
        time.sleep(0.05)
        with open(transcript, "a") as f:
            f.write(_claude_assistant_line(text="Hello world"))

    # Run streamer in a thread so we can append and then trigger exit.
    done = threading.Event()

    def runner():
        with patch.object(streamer, "daemon_post", side_effect=fake_post):
            streamer.run(transcript, "peer1", "%42", "session-1")
        done.set()

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    write_after_delay()
    # Let streamer pick up the line, then signal exit via pidfile removal.
    time.sleep(0.2)
    pid_path = streamer.streamer_pid_path("%42")
    if pid_path.exists():
        pid_path.unlink()
    done.wait(timeout=2.0)

    text_posts = [p for path, p in posts if path == "/events/chat_delta"]
    assert text_posts, "streamer should have posted at least one delta"
    assert text_posts[0]["kind"] == "text"
    assert text_posts[0]["text"] == "Hello world"
    assert text_posts[0]["chunk_index"] == 0
    assert text_posts[0]["peer"] == "peer1"
    assert text_posts[0]["pane_id"] == "%42"
    assert text_posts[0]["session_id"] == "session-1"
    assert text_posts[0]["turn_id"] == "uuid-1"


def test_streamer_emits_tool_use_delta(tmp_path):
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    posts: list[dict] = []

    def fake_post(_path: str, payload: dict, **_kw):
        posts.append(payload)
        return {}

    def runner():
        with patch.object(streamer, "daemon_post", side_effect=fake_post):
            streamer.run(transcript, "peer1", "%43")

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    time.sleep(0.05)
    with open(transcript, "a") as f:
        f.write(_claude_assistant_line(
            tool_use={"name": "Bash", "input": {"command": "ls -la"}},
        ))
    time.sleep(0.15)
    p = streamer.streamer_pid_path("%43")
    if p.exists():
        p.unlink()
    t.join(timeout=2.0)

    tool_posts = [p for p in posts if p.get("kind") == "tool_use"]
    assert tool_posts
    assert tool_posts[0]["tool_call"]["name"] == "Bash"
    assert "ls -la" in tool_posts[0]["tool_call"]["input"]


def test_streamer_skips_pre_existing_lines(tmp_path):
    """Lines written before the streamer starts must not be replayed as deltas."""
    transcript = tmp_path / "t.jsonl"
    with open(transcript, "w") as f:
        f.write(_claude_assistant_line(text="OLD turn from before"))
        f.write(_claude_assistant_line(text="Also old"))

    posts: list[dict] = []

    def fake_post(_path: str, payload: dict, **_kw):
        posts.append(payload)
        return {}

    def runner():
        with patch.object(streamer, "daemon_post", side_effect=fake_post):
            streamer.run(transcript, "peer1", "%44")

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    time.sleep(0.2)
    p = streamer.streamer_pid_path("%44")
    if p.exists():
        p.unlink()
    t.join(timeout=2.0)

    assert not posts, f"expected no deltas for pre-existing lines, got {posts}"


def test_streamer_exits_on_pidfile_removal(tmp_path):
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")

    def runner():
        with patch.object(streamer, "daemon_post", return_value={}):
            streamer.run(transcript, "peer1", "%45")

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    time.sleep(0.05)
    p = streamer.streamer_pid_path("%45")
    assert p.exists()
    p.unlink()
    t.join(timeout=2.0)
    assert not t.is_alive(), "streamer should exit after pidfile removal"


def test_streamer_pidfile_owner_check_on_cleanup(tmp_path):
    """Streamer must not delete a pidfile whose contents no longer name it.

    With the lock-based single-owner model this collision shouldn't happen in
    production, but the owner check is a defense-in-depth invariant for any
    out-of-band pidfile rewrite (manual ops, test harnesses, etc.).
    """
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    pid_path = streamer.streamer_pid_path("%46")

    def runner():
        with patch.object(streamer, "daemon_post", return_value={}):
            streamer.run(transcript, "peer1", "%46")

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    time.sleep(0.05)
    assert pid_path.exists()
    other_pid = os.getpid() + 99999
    pid_path.write_text(str(other_pid))
    # The streamer reads the pidfile inside its loop and returns immediately
    # once it sees a foreign pid (we don't even need to wait for idle).
    t.join(timeout=2.0)
    assert pid_path.exists(), "streamer must not delete pidfile owned by another process"
    assert pid_path.read_text().strip() == str(other_pid)
    pid_path.unlink()


def test_second_streamer_exits_when_lock_held(tmp_path):
    """Two streamers spawning for the same pane: the second must exit cleanly
    without touching the transcript or pidfile."""
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    streamer.IDLE_EXIT_AFTER_S = 5.0  # keep first alive

    posts: list[dict] = []

    def fake_post(_path: str, payload: dict, **_kw):
        posts.append(payload)
        return {}

    def runner_first():
        with patch.object(streamer, "daemon_post", side_effect=fake_post):
            streamer.run(transcript, "first", "%60")

    t1 = threading.Thread(target=runner_first, daemon=True)
    t1.start()
    time.sleep(0.1)
    pid_path = streamer.streamer_pid_path("%60")
    first_pid = pid_path.read_text().strip()
    assert first_pid

    # Second invocation in this same process — must observe lock held and
    # return 0 without overwriting pidfile.
    with patch.object(streamer, "daemon_post", side_effect=fake_post):
        rc = streamer.run(transcript, "second", "%60")
    assert rc == 0
    assert pid_path.read_text().strip() == first_pid, (
        "second streamer must not overwrite live owner's pidfile"
    )

    # Cleanup first.
    pid_path.unlink()
    t1.join(timeout=2.0)


def test_terminate_live_streamer_kills_predecessor(tmp_path):
    """prompt-hook's terminate_live_streamer() must signal the running streamer
    to exit before a fresh one spawns."""
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    streamer.IDLE_EXIT_AFTER_S = 5.0

    def runner():
        with patch.object(streamer, "daemon_post", return_value={}):
            streamer.run(transcript, "peer1", "%61")

    t = threading.Thread(target=runner, daemon=True)
    t.start()
    time.sleep(0.1)
    pid_path = streamer.streamer_pid_path("%61")
    assert pid_path.exists()

    # Terminate. Streamer is in-process — SIGTERM would kill the test runner,
    # so monkey-patch os.kill to a no-op; pidfile-removal alone exits the loop.
    with patch.object(streamer.os, "kill"):
        result = streamer.terminate_live_streamer("%61", wait_s=2.0)
    assert result is True
    t.join(timeout=2.0)
    assert not t.is_alive(), "streamer should exit after terminate_live_streamer"


def test_terminate_live_streamer_no_op_when_absent():
    assert streamer.terminate_live_streamer("%62") is False


def test_terminate_live_streamer_clears_stale_pidfile():
    """A pidfile for a dead pid should be cleared and reported absent."""
    pid_path = streamer.streamer_pid_path("%63")
    # PID guaranteed not to be alive (large and randomized — same trick as
    # the owner-check test).
    pid_path.write_text(str(2**31 - 1))
    assert streamer.terminate_live_streamer("%63") is False
    assert not pid_path.exists()


def test_has_live_streamer():
    """has_live_streamer probes pid liveness, not just pidfile presence."""
    assert streamer.has_live_streamer("%64") is False
    pid_path = streamer.streamer_pid_path("%64")
    pid_path.write_text(str(os.getpid()))
    try:
        assert streamer.has_live_streamer("%64") is True
    finally:
        pid_path.unlink()
    pid_path.write_text(str(2**31 - 1))
    try:
        assert streamer.has_live_streamer("%64") is False
    finally:
        if pid_path.exists():
            pid_path.unlink()


def test_streamer_handles_missing_transcript(tmp_path):
    """Missing transcript at run-time must exit cleanly, not crash."""
    missing = tmp_path / "missing.jsonl"
    with patch.object(streamer, "daemon_post", return_value={}):
        rc = streamer.run(missing, "peer1", "%47")
    assert rc == 0
    assert not streamer.streamer_pid_path("%47").exists()


def test_streamer_idle_exit_when_no_writes(tmp_path):
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    streamer.IDLE_EXIT_AFTER_S = 0.1
    with patch.object(streamer, "daemon_post", return_value={}):
        rc = streamer.run(transcript, "peer1", "%48")
    assert rc == 0
    assert not streamer.streamer_pid_path("%48").exists()


def test_main_argparse(tmp_path):
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("")
    streamer.IDLE_EXIT_AFTER_S = 0.05
    with patch.object(streamer, "daemon_post", return_value={}):
        rc = streamer.main([
            "--transcript", str(transcript),
            "--peer", "peer1",
            "--pane-id", "%49",
        ])
    assert rc == 0
