"""Filesystem install/uninstall tests for the Codex installer.

Codex stores hooks in ~/.codex/hooks.json (JSON) and the MCP server +
`[features] hooks = true` flag in ~/.codex/config.toml (string-edited).

Tests rebind the module-level path constants to a tmp_path so the real
filesystem is never touched. install_hooks / install_mcp must preserve
user content, be idempotent, and round-trip cleanly via the matching
uninstall.
"""

from __future__ import annotations

import json

from repowire.installers import codex as codex_mod


def _retarget(tmp_path, monkeypatch):
    """Point codex module's path constants at tmp_path."""
    home = tmp_path / ".codex"
    monkeypatch.setattr(codex_mod, "CODEX_HOME", home)
    monkeypatch.setattr(codex_mod, "HOOKS_PATH", home / "hooks.json")
    monkeypatch.setattr(codex_mod, "CONFIG_PATH", home / "config.toml")
    return home


# -- install_hooks ----------------------------------------------------------


def test_install_hooks_on_empty_writes_all_events(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    assert codex_mod.install_hooks() is True

    data = json.loads((home / "hooks.json").read_text())
    assert set(data["hooks"]) == {"SessionStart", "Stop", "UserPromptSubmit"}
    # Each event has exactly one repowire entry.
    for event in ("SessionStart", "Stop", "UserPromptSubmit"):
        entries = data["hooks"][event]
        assert len(entries) == 1
        assert "repowire" in entries[0]["hooks"][0]["command"]
    # SessionStart carries the documented matcher.
    assert data["hooks"]["SessionStart"][0]["matcher"] == "startup|resume|clear"


def test_install_hooks_preserves_user_entries(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    user_entry = {"hooks": [{"type": "command", "command": "/usr/local/bin/my-script"}]}
    pre = {"hooks": {"Stop": [user_entry], "OtherEvent": [user_entry]}, "user_key": "keep me"}
    (home / "hooks.json").write_text(json.dumps(pre))

    codex_mod.install_hooks()
    data = json.loads((home / "hooks.json").read_text())

    # User's Stop entry survives alongside the repowire one.
    stop = data["hooks"]["Stop"]
    assert user_entry in stop
    assert any("repowire" in e["hooks"][0]["command"] for e in stop)
    # Unrelated event is untouched.
    assert data["hooks"]["OtherEvent"] == [user_entry]
    # Top-level user keys preserved.
    assert data["user_key"] == "keep me"


def test_install_hooks_is_idempotent(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    codex_mod.install_hooks()
    first = (home / "hooks.json").read_text()
    codex_mod.install_hooks()
    second = (home / "hooks.json").read_text()

    assert first == second
    data = json.loads(second)
    for event in ("SessionStart", "Stop", "UserPromptSubmit"):
        repowire_entries = [
            e for e in data["hooks"][event]
            if "repowire" in e["hooks"][0]["command"]
        ]
        assert len(repowire_entries) == 1, f"duplicate repowire hook in {event}"


# -- uninstall_hooks --------------------------------------------------------


def test_uninstall_hooks_after_install_clears_repowire(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    codex_mod.install_hooks()
    assert codex_mod.uninstall_hooks() is True

    data = json.loads((home / "hooks.json").read_text())
    # The whole top-level "hooks" key is dropped when nothing remains.
    assert "hooks" not in data


def test_uninstall_hooks_keeps_user_entries(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    codex_mod.install_hooks()
    # Append a user entry to an existing event.
    data = json.loads((home / "hooks.json").read_text())
    user_entry = {"hooks": [{"type": "command", "command": "/usr/local/bin/my-script"}]}
    data["hooks"]["Stop"].append(user_entry)
    (home / "hooks.json").write_text(json.dumps(data))

    assert codex_mod.uninstall_hooks() is True
    after = json.loads((home / "hooks.json").read_text())
    assert after["hooks"]["Stop"] == [user_entry]
    # SessionStart and UserPromptSubmit had only repowire entries -> gone.
    assert "SessionStart" not in after["hooks"]
    assert "UserPromptSubmit" not in after["hooks"]


def test_uninstall_hooks_noop_when_absent(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    # No file at all.
    assert codex_mod.uninstall_hooks() is False


def test_uninstall_hooks_noop_when_only_user_entries(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    user_only = {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "echo hi"}]}]}}
    (home / "hooks.json").write_text(json.dumps(user_only))

    assert codex_mod.uninstall_hooks() is False
    assert json.loads((home / "hooks.json").read_text()) == user_only


# -- install_mcp / config.toml ----------------------------------------------


def test_install_mcp_on_empty_writes_section_and_feature_flag(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    assert codex_mod.install_mcp() is True
    content = (home / "config.toml").read_text()

    assert "[mcp_servers.repowire]" in content
    assert 'command = "repowire"' in content
    assert 'args = ["mcp"]' in content
    assert "[features]" in content
    assert "hooks = true" in content


def test_install_mcp_preserves_existing_config(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    pre = (
        "model = \"gpt-5\"\n"
        "\n"
        "[mcp_servers.other]\n"
        "command = \"other-tool\"\n"
        "args = [\"serve\"]\n"
    )
    (home / "config.toml").write_text(pre)

    codex_mod.install_mcp()
    content = (home / "config.toml").read_text()

    # Existing content survives verbatim.
    assert "model = \"gpt-5\"" in content
    assert "[mcp_servers.other]" in content
    assert "command = \"other-tool\"" in content
    # And ours is appended.
    assert "[mcp_servers.repowire]" in content


def test_install_mcp_is_idempotent(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    codex_mod.install_mcp()
    first = (home / "config.toml").read_text()
    codex_mod.install_mcp()
    second = (home / "config.toml").read_text()

    assert first == second
    assert second.count("[mcp_servers.repowire]") == 1
    assert second.count("hooks = true") == 1


def test_install_mcp_respects_existing_features_block(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    (home / "config.toml").write_text("[features]\nhooks = false\n")

    codex_mod.install_mcp()
    content = (home / "config.toml").read_text()
    # Existing hooks flag respected (not overwritten).
    assert "hooks = false" in content
    assert "hooks = true" not in content


def test_install_mcp_injects_into_existing_features_block(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    (home / "config.toml").write_text("[features]\nother = true\n")

    codex_mod.install_mcp()
    content = (home / "config.toml").read_text()
    assert "hooks = true" in content
    assert "other = true" in content
    # Only one [features] header.
    assert content.count("[features]") == 1


# -- uninstall_mcp ----------------------------------------------------------


def test_uninstall_mcp_removes_section(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)

    codex_mod.install_mcp()
    assert codex_mod.uninstall_mcp() is True
    content = (home / "config.toml").read_text()
    assert "[mcp_servers.repowire]" not in content
    assert "command = \"repowire\"" not in content


def test_uninstall_mcp_keeps_other_sections(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    pre = (
        "model = \"gpt-5\"\n"
        "\n"
        "[mcp_servers.other]\n"
        "command = \"other-tool\"\n"
    )
    (home / "config.toml").write_text(pre)
    codex_mod.install_mcp()

    codex_mod.uninstall_mcp()
    content = (home / "config.toml").read_text()
    assert "[mcp_servers.other]" in content
    assert "command = \"other-tool\"" in content
    assert "model = \"gpt-5\"" in content


def test_uninstall_mcp_noop_when_absent(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    # No config.toml at all.
    assert codex_mod.uninstall_mcp() is False


def test_uninstall_mcp_noop_when_section_missing(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    (home / "config.toml").write_text("model = \"gpt-5\"\n")
    assert codex_mod.uninstall_mcp() is False


# -- check_* ----------------------------------------------------------------


def test_check_hooks_installed_reflects_state(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    assert codex_mod.check_hooks_installed() is False
    codex_mod.install_hooks()
    assert codex_mod.check_hooks_installed() is True
    codex_mod.uninstall_hooks()
    assert codex_mod.check_hooks_installed() is False


def test_check_mcp_installed_reflects_state(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    assert codex_mod.check_mcp_installed() is False
    codex_mod.install_mcp()
    assert codex_mod.check_mcp_installed() is True
    codex_mod.uninstall_mcp()
    assert codex_mod.check_mcp_installed() is False
