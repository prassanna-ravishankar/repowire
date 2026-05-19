"""Tests for serve --no-install-hooks guard (issue #209).

The daemon's startup lifespan installs server-global tmux hooks. Smoke/test
daemons running under a temp HOME need a way to opt out so they don't rewrite
the user's live mesh hooks.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from click.testing import CliRunner

from repowire.config.models import Config, DaemonConfig


@pytest.fixture()
def isolated_config(tmp_path: Path) -> Config:
    cfg = Config()
    cfg.daemon = DaemonConfig(host="127.0.0.1", port=19377)
    cfg._config_path = tmp_path / "config.yaml"
    return cfg


@pytest.mark.asyncio
async def test_create_app_skips_tmux_hooks_when_disabled(monkeypatch, tmp_path, isolated_config):
    """install_tmux_hooks=False must prevent tmux_lifecycle.install_hooks from running."""
    from repowire.daemon import app as app_mod

    monkeypatch.setattr(Path, "home", lambda: tmp_path)

    called: list[tuple[str, int]] = []

    def fake_install_hooks(host: str, port: int):
        called.append((host, port))
        return ["fake-hook"]

    def fake_is_tmux_available() -> bool:
        return True

    import repowire.hooks.tmux_lifecycle as tmux_mod
    monkeypatch.setattr(tmux_mod, "install_hooks", fake_install_hooks)
    monkeypatch.setattr(tmux_mod, "is_tmux_available", fake_is_tmux_available)

    app = app_mod.create_app(config=isolated_config, install_tmux_hooks=False)
    async with app.router.lifespan_context(app):
        pass

    assert called == [], "install_hooks should not be called when install_tmux_hooks=False"


@pytest.mark.asyncio
async def test_create_app_installs_tmux_hooks_by_default(monkeypatch, tmp_path, isolated_config):
    """Default behavior must remain unchanged: tmux hooks install when available."""
    from repowire.daemon import app as app_mod

    monkeypatch.setattr(Path, "home", lambda: tmp_path)

    called: list[tuple[str, int]] = []

    def fake_install_hooks(host: str, port: int):
        called.append((host, port))
        return ["fake-hook"]

    import repowire.hooks.tmux_lifecycle as tmux_mod
    monkeypatch.setattr(tmux_mod, "install_hooks", fake_install_hooks)
    monkeypatch.setattr(tmux_mod, "is_tmux_available", lambda: True)

    app = app_mod.create_app(config=isolated_config)
    async with app.router.lifespan_context(app):
        pass

    assert called == [("127.0.0.1", 19377)]


def test_serve_cli_passes_no_install_hooks_flag(monkeypatch):
    """`repowire serve --no-install-hooks` must call create_app(install_tmux_hooks=False)."""
    from repowire import cli as cli_mod

    captured: dict[str, object] = {}

    def fake_create_app(config=None, install_tmux_hooks=True):
        captured["install_tmux_hooks"] = install_tmux_hooks

        class _Stub:
            pass
        return _Stub()

    def fake_uvicorn_run(app, **kwargs):
        captured["ran"] = True

    import repowire.daemon.app as app_mod
    monkeypatch.setattr(app_mod, "create_app", fake_create_app)

    import uvicorn
    monkeypatch.setattr(uvicorn, "run", fake_uvicorn_run)

    runner = CliRunner()
    result = runner.invoke(cli_mod.main, ["serve", "--no-install-hooks", "--port", "19377"])
    assert result.exit_code == 0, result.output
    assert captured.get("install_tmux_hooks") is False
    assert captured.get("ran") is True


def test_serve_cli_env_var_disables_hooks(monkeypatch):
    """REPOWIRE_DISABLE_HOOK_INSTALL=1 must disable hook install without the CLI flag."""
    from repowire import cli as cli_mod

    captured: dict[str, object] = {}

    def fake_create_app(config=None, install_tmux_hooks=True):
        captured["install_tmux_hooks"] = install_tmux_hooks

        class _Stub:
            pass
        return _Stub()

    def fake_uvicorn_run(app, **kwargs):
        captured["ran"] = True

    import repowire.daemon.app as app_mod
    monkeypatch.setattr(app_mod, "create_app", fake_create_app)

    import uvicorn
    monkeypatch.setattr(uvicorn, "run", fake_uvicorn_run)

    monkeypatch.setenv("REPOWIRE_DISABLE_HOOK_INSTALL", "1")
    runner = CliRunner()
    result = runner.invoke(cli_mod.main, ["serve", "--port", "19377"])
    assert result.exit_code == 0, result.output
    assert captured.get("install_tmux_hooks") is False


def test_serve_cli_default_installs_hooks(monkeypatch):
    """Default `repowire serve` must keep install_tmux_hooks=True."""
    from repowire import cli as cli_mod

    captured: dict[str, object] = {}

    def fake_create_app(config=None, install_tmux_hooks=True):
        captured["install_tmux_hooks"] = install_tmux_hooks

        class _Stub:
            pass
        return _Stub()

    def fake_uvicorn_run(app, **kwargs):
        captured["ran"] = True

    import repowire.daemon.app as app_mod
    monkeypatch.setattr(app_mod, "create_app", fake_create_app)

    import uvicorn
    monkeypatch.setattr(uvicorn, "run", fake_uvicorn_run)

    monkeypatch.delenv("REPOWIRE_DISABLE_HOOK_INSTALL", raising=False)
    runner = CliRunner()
    result = runner.invoke(cli_mod.main, ["serve", "--port", "19377"])
    assert result.exit_code == 0, result.output
    assert captured.get("install_tmux_hooks") is True
