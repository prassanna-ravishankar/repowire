"""Tests for the Antigravity CLI (`agy`) plugin installer.

The installer file-drops a plugin directory at
`~/.gemini/antigravity-cli/plugins/repowire/` and updates
`import_manifest.json`. Tests retarget those paths to tmp_path so the real
filesystem is never touched.

Limitation tests below make the unverified-upstream gap explicit:
`check_mcp_installed()` always returns False, and `install_hooks()` only
writes the verified-plugin layout (plugin.json + hooks/hooks.json) without
attempting any MCP-server JSON the Antigravity plugin schema hasn't
documented yet.
"""

from __future__ import annotations

import json

from repowire.installers import antigravity as agy_mod


def _retarget(tmp_path, monkeypatch):
    home = tmp_path / ".gemini" / "antigravity-cli"
    monkeypatch.setattr(agy_mod, "ANTIGRAVITY_HOME", home)
    monkeypatch.setattr(agy_mod, "PLUGINS_DIR", home / "plugins")
    monkeypatch.setattr(agy_mod, "MANIFEST_PATH", home / "import_manifest.json")
    return home


def _read_manifest(home):
    return json.loads((home / "import_manifest.json").read_text())


def _read_hooks(home):
    return json.loads(
        (home / "plugins" / "repowire" / "hooks" / "hooks.json").read_text()
    )


def _read_plugin(home):
    return json.loads((home / "plugins" / "repowire" / "plugin.json").read_text())


# -- install_hooks ----------------------------------------------------------


def test_install_hooks_writes_plugin_dir_and_manifest(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    assert agy_mod.install_hooks() is True

    plugin = _read_plugin(home)
    assert plugin["name"] == "repowire"
    assert "version" in plugin

    hooks = _read_hooks(home)
    assert set(hooks) == {"SessionStart", "BeforeAgent", "AfterAgent"}
    for event in ("SessionStart", "BeforeAgent", "AfterAgent"):
        cmd = hooks[event][0]["hooks"][0]["command"]
        assert "repowire hook" in cmd
        assert "--backend=antigravity" in cmd
    assert hooks["SessionStart"][0]["matcher"] == "startup"

    manifest = _read_manifest(home)
    repowire_entries = [e for e in manifest["imports"] if e["name"] == "repowire"]
    assert len(repowire_entries) == 1
    assert repowire_entries[0]["source"] == "local-install"


def test_install_hooks_is_idempotent(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    agy_mod.install_hooks()
    agy_mod.install_hooks()
    manifest = _read_manifest(home)
    repowire_entries = [e for e in manifest["imports"] if e["name"] == "repowire"]
    assert len(repowire_entries) == 1


def test_install_hooks_preserves_other_manifest_entries(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    pre = {
        "imports": [
            {"name": "other-plugin", "source": "local-install",
             "importedAt": "2026-05-01T00:00:00Z", "components": ["installed"]},
        ],
    }
    (home / "import_manifest.json").write_text(json.dumps(pre))

    agy_mod.install_hooks()
    manifest = _read_manifest(home)
    names = {e["name"] for e in manifest["imports"]}
    assert names == {"other-plugin", "repowire"}


def test_install_hooks_does_not_write_mcp_servers_dir(tmp_path, monkeypatch):
    """LIMITATION: the Antigravity plugin schema for mcpServers is not
    verified, so the installer must not create that subdirectory.
    """
    home = _retarget(tmp_path, monkeypatch)
    agy_mod.install_hooks()
    assert not (home / "plugins" / "repowire" / "mcpServers").exists()


# -- uninstall_hooks --------------------------------------------------------


def test_uninstall_hooks_removes_plugin_dir_and_manifest_entry(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    agy_mod.install_hooks()
    assert agy_mod.uninstall_hooks() is True
    assert not (home / "plugins" / "repowire").exists()
    assert not (home / "import_manifest.json").exists()


def test_uninstall_hooks_keeps_other_manifest_entries(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    pre = {
        "imports": [
            {"name": "other-plugin", "source": "local-install",
             "importedAt": "2026-05-01T00:00:00Z", "components": ["installed"]},
        ],
    }
    (home / "import_manifest.json").write_text(json.dumps(pre))
    agy_mod.install_hooks()

    assert agy_mod.uninstall_hooks() is True
    manifest = _read_manifest(home)
    assert manifest["imports"] == [
        {"name": "other-plugin", "source": "local-install",
         "importedAt": "2026-05-01T00:00:00Z", "components": ["installed"]},
    ]


def test_uninstall_hooks_noop_when_absent(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    assert agy_mod.uninstall_hooks() is False


def test_install_handles_null_imports_left_by_agy(tmp_path, monkeypatch):
    """`agy plugin uninstall` writes {"imports": null}; the installer must
    treat that as an empty list rather than crashing on iteration.
    """
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    (home / "import_manifest.json").write_text(json.dumps({"imports": None}))
    assert agy_mod.install_hooks() is True
    assert agy_mod.check_hooks_installed() is True


# -- check_* ----------------------------------------------------------------


def test_check_hooks_installed_reflects_state(tmp_path, monkeypatch):
    _retarget(tmp_path, monkeypatch)
    assert agy_mod.check_hooks_installed() is False
    agy_mod.install_hooks()
    assert agy_mod.check_hooks_installed() is True
    agy_mod.uninstall_hooks()
    assert agy_mod.check_hooks_installed() is False


def test_check_mcp_installed_always_false_pending_upstream(tmp_path, monkeypatch):
    """LIMITATION: Antigravity MCP plugin schema is not yet verified. The
    installer never writes mcpServers entries, so check_mcp_installed() must
    return False even when hooks are installed.
    """
    _retarget(tmp_path, monkeypatch)
    assert agy_mod.check_mcp_installed() is False
    agy_mod.install_hooks()
    assert agy_mod.check_mcp_installed() is False


# -- Adapter ---------------------------------------------------------------


def test_hook_output_emits_allow_for_antigravity(capsys):
    from repowire.hooks import adapters

    adapters.hook_output("antigravity")
    captured = capsys.readouterr().out.strip()
    assert json.loads(captured) == {"decision": "allow"}


def test_hook_output_no_emit_for_claude_code(capsys):
    from repowire.hooks import adapters

    adapters.hook_output("claude-code")
    assert capsys.readouterr().out == ""


# -- AgentType + spawn -----------------------------------------------------


def test_antigravity_agent_type_registered():
    from repowire.config.models import DEFAULT_SPAWN_COMMANDS, AgentType

    assert AgentType.ANTIGRAVITY.value == "antigravity"
    assert "agy" in DEFAULT_SPAWN_COMMANDS[AgentType.ANTIGRAVITY]


def test_legacy_allowed_commands_maps_agy_to_antigravity():
    from repowire.config.models import AgentType, SpawnSettings

    s = SpawnSettings(allowed_commands=["agy --dangerously-skip-permissions"])
    assert s.commands[AgentType.ANTIGRAVITY] == "agy --dangerously-skip-permissions"


def test_peer_new_cli_choice_includes_antigravity():
    """`repowire peer new --backend=antigravity` must be an accepted choice."""
    from repowire.cli import peer_new

    backend_param = next(p for p in peer_new.params if p.name == "backend")
    assert "antigravity" in backend_param.type.choices


def test_peer_new_cli_exposes_profile_option():
    """`repowire peer new` should expose named spawn profiles."""
    from repowire.cli import peer_new

    assert any(p.name == "profile" for p in peer_new.params)


# -- Round-trip -------------------------------------------------------------


def test_full_roundtrip_restores_user_manifest(tmp_path, monkeypatch):
    home = _retarget(tmp_path, monkeypatch)
    home.mkdir(parents=True)
    original = {
        "imports": [
            {"name": "other", "source": "local-install",
             "importedAt": "2026-05-01T00:00:00Z", "components": ["installed"]},
        ],
    }
    (home / "import_manifest.json").write_text(json.dumps(original))

    agy_mod.install_hooks()
    agy_mod.uninstall_hooks()

    assert _read_manifest(home) == original
    assert not (home / "plugins" / "repowire").exists()
