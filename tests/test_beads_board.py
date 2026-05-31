"""Read-only Beads board snapshot + route (repowire-n4q).

Read-only: only ``bd ready`` / ``bd list`` are ever shelled (no write/sync
verb). Degrades to ``available=false`` when bd is missing / no workspace /
lookups fail. Groups are bounded so a large backlog can't bloat the payload.
"""

from __future__ import annotations

import json
import subprocess

import pytest

from repowire.daemon import beads_board as bb
from repowire.daemon.routes import beads as beads_route

from .conftest import async_client_for, make_daemon_app


def _row(rid: str, status: str, **over) -> dict:
    base = {
        "id": rid,
        "title": f"title {rid}",
        "status": status,
        "priority": 2,
        "issue_type": "task",
        "assignee": None,
        "updated_at": "2026-05-31T00:00:00Z",
    }
    base.update(over)
    return base


def _fake_bd(monkeypatch, *, by_status: dict[str, list[dict]], record: list | None = None):
    """Stub subprocess.run for bd calls; route args by the command/status."""

    def fake_run(cmd, **_kw):
        if record is not None:
            record.append(cmd)
        # cmd = ["bd", <verb>, ...maybe --status=X..., "--json"]
        if cmd[1] == "ready":
            rows = by_status.get("ready", [])
        else:
            status = next(
                (a.split("=", 1)[1] for a in cmd if a.startswith("--status=")), ""
            )
            rows = by_status.get(status, [])
        return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(rows), stderr="")

    monkeypatch.setattr(bb.subprocess, "run", fake_run)
    monkeypatch.setattr(bb.shutil, "which", lambda _x: "/usr/local/bin/bd")
    monkeypatch.setattr(bb, "_find_beads_root", lambda start=None: bb.Path("/repo"))


# --- read_beads_board ---


def test_board_unavailable_when_bd_missing(monkeypatch):
    monkeypatch.setattr(bb.shutil, "which", lambda _x: None)
    board = bb.read_beads_board()
    assert board["available"] is False
    assert board["ready"]["items"] == []


def test_board_unavailable_when_no_workspace(monkeypatch):
    monkeypatch.setattr(bb.shutil, "which", lambda _x: "/bin/bd")
    monkeypatch.setattr(bb, "_find_beads_root", lambda start=None: None)
    assert bb.read_beads_board()["available"] is False


def test_board_groups_and_slims_rows(monkeypatch):
    _fake_bd(
        monkeypatch,
        by_status={
            "ready": [_row("a", "open")],
            "in_progress": [_row("b", "in_progress", assignee="me@x.io")],
            "blocked": [],
            "closed": [
                _row("c", "closed", updated_at="2026-05-30T00:00:00Z"),
                _row("d", "closed", updated_at="2026-05-31T00:00:00Z"),
            ],
        },
    )
    board = bb.read_beads_board()
    assert board["available"] is True
    assert [r["id"] for r in board["ready"]["items"]] == ["a"]
    assert board["in_progress"]["items"][0]["assignee"] == "me@x.io"
    # recently_closed sorted by updated_at desc.
    assert [r["id"] for r in board["recently_closed"]["items"]] == ["d", "c"]
    # slim shape: no description/labels leak through.
    assert set(board["ready"]["items"][0]) == {
        "id", "title", "status", "priority", "issue_type", "assignee"
    }


def test_groups_are_bounded_with_truncation_flag(monkeypatch):
    many = [_row(f"r{i}", "open") for i in range(bb._GROUP_LIMIT + 5)]
    _fake_bd(monkeypatch, by_status={"ready": many})
    board = bb.read_beads_board()
    assert len(board["ready"]["items"]) == bb._GROUP_LIMIT
    assert board["ready"]["total"] == bb._GROUP_LIMIT + 5
    assert board["ready"]["truncated"] is True


def test_only_read_verbs_are_shelled(monkeypatch):
    calls: list[list[str]] = []
    _fake_bd(monkeypatch, by_status={}, record=calls)
    bb.read_beads_board()
    verbs = {c[1] for c in calls}
    assert verbs <= {"ready", "list"}  # never a write/sync verb
    # And every call is read-only with --json.
    assert all("--json" in c for c in calls)
    assert not any(v in c for c in calls for v in ("close", "create", "update", "dolt"))


def test_board_unavailable_when_every_lookup_fails(monkeypatch):
    monkeypatch.setattr(bb.shutil, "which", lambda _x: "/bin/bd")
    monkeypatch.setattr(bb, "_find_beads_root", lambda start=None: bb.Path("/repo"))
    monkeypatch.setattr(
        bb.subprocess,
        "run",
        lambda *a, **k: subprocess.CompletedProcess(a, 1, stdout="", stderr="boom"),
    )
    assert bb.read_beads_board()["available"] is False


# --- GET /beads/board ---


@pytest.mark.asyncio
async def test_board_route_returns_snapshot_and_caches(tmp_path, monkeypatch):
    beads_route._cache = None  # reset module cache between tests
    calls = {"n": 0}

    def fake_read():
        calls["n"] += 1
        return bb._empty_board(available=True)

    monkeypatch.setattr(beads_route, "read_beads_board", fake_read)

    harness = make_daemon_app(tmp_path, [beads_route.router])
    async with async_client_for(harness.app) as client:
        r1 = await client.get("/beads/board")
        r2 = await client.get("/beads/board")
    assert r1.status_code == 200
    assert r1.json()["available"] is True
    # Second call within the TTL is served from cache (no extra bd read).
    assert calls["n"] == 1
    assert r2.json()["available"] is True
