"""Shared installer behavior contracts."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest

from repowire.installers import codex as codex_mod
from repowire.installers import gemini as gemini_mod


@dataclass(frozen=True)
class JsonHooksInstaller:
    name: str
    module: Any
    home_dir: str
    settings_path: str
    filename: str
    events: tuple[str, ...]
    user_event: str
    custom_event: str
    session_matcher: str
    backend_flag: str | None = None

    def retarget(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        home = tmp_path / self.home_dir
        monkeypatch.setattr(self.module, self.home_dir_attr, home)
        monkeypatch.setattr(self.module, self.settings_path, home / self.filename)
        return home

    @property
    def home_dir_attr(self) -> str:
        return {
            "codex": "CODEX_HOME",
            "gemini": "GEMINI_HOME",
        }[self.name]

    def read(self, home: Path) -> dict:
        return json.loads((home / self.filename).read_text())


JSON_HOOK_INSTALLERS = (
    JsonHooksInstaller(
        name="codex",
        module=codex_mod,
        home_dir=".codex",
        settings_path="HOOKS_PATH",
        filename="hooks.json",
        events=("SessionStart", "SessionEnd", "Stop", "UserPromptSubmit"),
        user_event="Stop",
        custom_event="OtherEvent",
        session_matcher="startup|resume|clear",
    ),
    JsonHooksInstaller(
        name="gemini",
        module=gemini_mod,
        home_dir=".gemini",
        settings_path="SETTINGS_PATH",
        filename="settings.json",
        events=("SessionStart", "BeforeAgent", "AfterAgent"),
        user_event="AfterAgent",
        custom_event="CustomEvent",
        session_matcher="startup",
        backend_flag="--backend=gemini",
    ),
)


@pytest.fixture(params=JSON_HOOK_INSTALLERS, ids=lambda spec: spec.name)
def json_hooks_installer(request):
    return request.param


def test_install_hooks_on_empty_writes_expected_events(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)

    assert spec.module.install_hooks() is True
    data = spec.read(home)

    assert set(data["hooks"]) == set(spec.events)
    for event in spec.events:
        entries = data["hooks"][event]
        assert len(entries) == 1
        command = entries[0]["hooks"][0]["command"]
        assert "repowire" in command
        if spec.backend_flag:
            assert spec.backend_flag in command
    assert data["hooks"]["SessionStart"][0]["matcher"] == spec.session_matcher


def test_install_hooks_preserves_user_entries_and_top_level_keys(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    user_entry = {"hooks": [{"type": "command", "command": "/usr/local/bin/my-hook"}]}
    before = {
        "user_key": "keep me",
        "hooks": {
            spec.user_event: [user_entry],
            spec.custom_event: [user_entry],
        },
    }
    (home / spec.filename).write_text(json.dumps(before))

    spec.module.install_hooks()
    data = spec.read(home)

    assert data["user_key"] == "keep me"
    assert user_entry in data["hooks"][spec.user_event]
    assert any(
        "repowire" in entry["hooks"][0]["command"]
        for entry in data["hooks"][spec.user_event]
    )
    assert data["hooks"][spec.custom_event] == [user_entry]


def test_install_hooks_is_idempotent(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)

    spec.module.install_hooks()
    first = (home / spec.filename).read_text()
    spec.module.install_hooks()
    second = (home / spec.filename).read_text()

    assert first == second
    data = json.loads(second)
    for event in spec.events:
        repowire_entries = [
            entry for entry in data["hooks"][event]
            if "repowire" in entry["hooks"][0]["command"]
        ]
        assert len(repowire_entries) == 1


def test_uninstall_hooks_after_install_drops_empty_hooks_key(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)

    spec.module.install_hooks()
    assert spec.module.uninstall_hooks() is True

    assert "hooks" not in spec.read(home)


def test_uninstall_hooks_keeps_user_entries(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)
    spec.module.install_hooks()
    data = spec.read(home)
    user_entry = {"hooks": [{"type": "command", "command": "echo hi"}]}
    data["hooks"][spec.user_event].append(user_entry)
    (home / spec.filename).write_text(json.dumps(data))

    assert spec.module.uninstall_hooks() is True

    after = spec.read(home)
    assert after["hooks"][spec.user_event] == [user_entry]
    for event in set(spec.events) - {spec.user_event}:
        assert event not in after["hooks"]


def test_uninstall_hooks_noops(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    home = spec.retarget(tmp_path, monkeypatch)

    assert spec.module.uninstall_hooks() is False

    home.mkdir(parents=True)
    user_only = {
        "hooks": {
            spec.user_event: [{"hooks": [{"type": "command", "command": "echo hi"}]}],
        },
    }
    (home / spec.filename).write_text(json.dumps(user_only))

    assert spec.module.uninstall_hooks() is False
    assert spec.read(home) == user_only


def test_check_hooks_installed_reflects_state(
    json_hooks_installer: JsonHooksInstaller,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    spec = json_hooks_installer
    spec.retarget(tmp_path, monkeypatch)

    assert spec.module.check_hooks_installed() is False
    spec.module.install_hooks()
    assert spec.module.check_hooks_installed() is True
    spec.module.uninstall_hooks()
    assert spec.module.check_hooks_installed() is False
