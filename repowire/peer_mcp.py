"""Per-peer MCP server config: list/add/remove across backends.

Same-host only for v1 — cross-host requires an ACP transport (not yet wired).
The daemon route layer enforces locality (peer.machine == this host) and circle
disambiguation; this module is pure backend-dispatch and is safe to call from
tests with mocked subprocesses or tmp_path-rooted config files.

Each backend exposes:

  list_servers(peer)         -> list[McpServerEntry]
  add_server(peer, spec)     -> None      (raises DuplicateServerError / NotSupportedError)
  remove_server(peer, name)  -> None      (raises ServerNotFoundError / NotSupportedError)

Errors are normalized into a small exception hierarchy so the route layer can
map them to consistent HTTP status codes (404, 409, 501, 504).
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from repowire.agent_backends import agent_backend_for
from repowire.protocol.peers import Peer

# Subprocess timeout for `claude mcp` calls. Surfaces as 504 in the route.
SUBPROCESS_TIMEOUT_SECONDS = 10


# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------


@dataclass
class McpServerEntry:
    """One configured MCP server, normalized across backends."""

    name: str
    scope: str = "user"  # user | project (claude-code distinguishes)
    type: str = "stdio"  # stdio | http | sse
    command: str | None = None
    args: list[str] = field(default_factory=list)
    url: str | None = None
    env_keys: list[str] = field(default_factory=list)  # keys only, values redacted

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "scope": self.scope,
            "type": self.type,
            "command": self.command,
            "args": list(self.args),
            "url": self.url,
            "env_keys": list(self.env_keys),
        }


@dataclass
class McpServerSpec:
    """Input shape for add_server. Pass-through, no schema validation."""

    name: str
    type: str = "stdio"
    command: str | None = None
    args: list[str] = field(default_factory=list)
    url: str | None = None
    env: dict[str, str] = field(default_factory=dict)
    scope: str = "user"


# ---------------------------------------------------------------------------
# Exceptions
# ---------------------------------------------------------------------------


class PeerMcpError(Exception):
    """Base error for peer MCP operations."""


class NotSupportedError(PeerMcpError):
    """Backend does not support MCP configuration in v1 (e.g. pi)."""


class DuplicateServerError(PeerMcpError):
    """Server with this name already exists."""


class ServerNotFoundError(PeerMcpError):
    """Named server is not configured."""


class BackendTimeoutError(PeerMcpError):
    """Backend CLI call timed out."""


class BackendError(PeerMcpError):
    """Backend CLI returned non-zero or unparseable output."""


class ValidationError(PeerMcpError):
    """MCP server input is unsafe or malformed."""


# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------


_SERVER_NAME_RE = re.compile(r"^[A-Za-z0-9_-]{1,128}$")
_ENV_KEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_RESERVED_SERVER_NAMES = {"__proto__", "constructor", "prototype"}


def _validate_server_name(name: str) -> None:
    if not name or not name.strip():
        raise ValidationError("server name is required")
    if name in _RESERVED_SERVER_NAMES:
        raise ValidationError(f"server name {name!r} is reserved")
    if not _SERVER_NAME_RE.fullmatch(name):
        raise ValidationError(
            "server name must contain only letters, numbers, underscores, or hyphens"
        )


def _validate_env_keys(env: dict[str, str]) -> None:
    for key in env:
        if not _ENV_KEY_RE.fullmatch(key):
            raise ValidationError(
                "env keys must start with a letter or underscore and contain only "
                "letters, numbers, or underscores"
            )


def _atomic_write_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    tmp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            "w",
            encoding="utf-8",
            dir=path.parent,
            prefix=f".{path.name}.",
            suffix=".tmp",
            delete=False,
        ) as tmp:
            tmp_path = Path(tmp.name)
            tmp.write(text)
            tmp.flush()
            os.fsync(tmp.fileno())
        tmp_path.chmod(mode)
        os.replace(tmp_path, path)
        try:
            dir_fd = os.open(path.parent, os.O_DIRECTORY)
        except OSError:
            return
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if tmp_path is not None and tmp_path.exists():
            tmp_path.unlink()


def list_servers(peer: Peer) -> list[McpServerEntry]:
    return agent_backend_for(peer.backend).list_mcp_servers(peer)


def add_server(peer: Peer, spec: McpServerSpec) -> None:
    _validate_server_name(spec.name)
    _validate_env_keys(spec.env)
    existing = {s.name for s in list_servers(peer)}
    if spec.name in existing:
        raise DuplicateServerError(f"server {spec.name!r} already exists")

    agent_backend_for(peer.backend).add_mcp_server(peer, spec)


def remove_server(peer: Peer, name: str) -> None:
    _validate_server_name(name)
    existing = {s.name for s in list_servers(peer)}
    if name not in existing:
        raise ServerNotFoundError(f"server {name!r} not configured")

    agent_backend_for(peer.backend).remove_mcp_server(peer, name)


# ---------------------------------------------------------------------------
# claude-code: shells out to the `claude mcp` CLI
# ---------------------------------------------------------------------------


def _claude_cli() -> str:
    path = shutil.which("claude")
    if not path:
        raise NotSupportedError("`claude` CLI not found on PATH")
    return path


def _run_claude(args: list[str], cwd: str | None = None) -> subprocess.CompletedProcess:
    cli = _claude_cli()
    try:
        return subprocess.run(
            [cli, *args],
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=SUBPROCESS_TIMEOUT_SECONDS,
            check=False,
        )
    except subprocess.TimeoutExpired as e:
        raise BackendTimeoutError(f"`claude {' '.join(args)}` timed out") from e


def _claude_list(peer: Peer) -> list[McpServerEntry]:
    """Use `claude mcp list` (text output). JSON flag isn't stable across
    versions, so we parse the human-readable list — one line per server like:

        servername: command arg1 arg2 - ✓ Connected
        urlname: https://example.com (SSE) - ✓ Connected
    """
    result = _run_claude(["mcp", "list"], cwd=peer.path or None)
    if result.returncode != 0:
        # CLI exits non-zero when no servers configured on some versions
        stdout_lower = result.stdout.lower()
        stderr_lower = result.stderr.lower()
        if "no mcp servers" in stdout_lower or "no mcp servers" in stderr_lower:
            return []
        detail = result.stderr.strip() or result.stdout.strip()
        raise BackendError(f"claude mcp list failed: {detail}")

    out: list[McpServerEntry] = []
    for line in result.stdout.splitlines():
        line = line.strip()
        if not line or ":" not in line:
            continue
        # Skip header/footer lines that don't have the "name: detail" shape
        name, _, rest = line.partition(":")
        name = name.strip()
        if not name or " " in name:
            continue
        rest = rest.strip()
        # Strip trailing status decorations like " - ✓ Connected"
        rest = re.sub(r"\s*-\s*[✓✗].*$", "", rest).strip()
        if rest.startswith(("http://", "https://")):
            url = rest.split()[0]
            entry = McpServerEntry(name=name, type="http", url=url)
        else:
            parts = rest.split()
            command = parts[0] if parts else None
            args = parts[1:] if len(parts) > 1 else []
            entry = McpServerEntry(name=name, type="stdio", command=command, args=args)
        out.append(entry)
    return out


def _claude_add(peer: Peer, spec: McpServerSpec) -> None:
    scope_flag = ["-s", "project"] if spec.scope == "project" else ["-s", "user"]
    env_flags: list[str] = []
    for k, v in spec.env.items():
        env_flags.extend(["-e", f"{k}={v}"])

    if spec.type in ("http", "sse"):
        if not spec.url:
            raise BackendError("url is required for http/sse type")
        transport = ["--transport", spec.type]
        args = ["mcp", "add", *scope_flag, *transport, *env_flags, spec.name, spec.url]
    else:
        if not spec.command:
            raise BackendError("command is required for stdio type")
        args = ["mcp", "add", *scope_flag, *env_flags, spec.name, "--", spec.command, *spec.args]

    cwd = peer.path if spec.scope == "project" else None
    result = _run_claude(args, cwd=cwd)
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip()
        raise BackendError(f"claude mcp add failed: {detail}")


def _claude_remove(peer: Peer, name: str) -> None:
    result = _run_claude(["mcp", "remove", name], cwd=peer.path or None)
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip()
        raise BackendError(f"claude mcp remove failed: {detail}")


# ---------------------------------------------------------------------------
# codex: edit ~/.codex/config.toml
# ---------------------------------------------------------------------------

CODEX_CONFIG_PATH = Path.home() / ".codex" / "config.toml"


_SECTION_RE = re.compile(r"^\[mcp_servers\.([^\]]+)\]\s*$")
_KV_RE = re.compile(r'^(\w+)\s*=\s*(.+?)\s*$')


def _parse_toml_value(raw: str) -> Any:
    """Minimal TOML value parser: handles strings, lists of strings, inline tables."""
    raw = raw.strip()
    if raw.startswith('"') and raw.endswith('"'):
        return raw[1:-1]
    if raw.startswith("[") and raw.endswith("]"):
        inner = raw[1:-1].strip()
        if not inner:
            return []
        items = re.findall(r'"([^"]*)"', inner)
        return items
    if raw.startswith("{") and raw.endswith("}"):
        out: dict[str, str] = {}
        for pair in re.findall(r'(\w+)\s*=\s*"([^"]*)"', raw):
            out[pair[0]] = pair[1]
        return out
    return raw


def _codex_read_sections() -> dict[str, dict[str, Any]]:
    """Parse only `[mcp_servers.NAME]` sections. Returns name -> {key: parsed_value}."""
    if not CODEX_CONFIG_PATH.exists():
        return {}
    sections: dict[str, dict[str, Any]] = {}
    current: str | None = None
    for line in CODEX_CONFIG_PATH.read_text().splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        m = _SECTION_RE.match(stripped)
        if m:
            current = m.group(1)
            sections[current] = {}
            continue
        if stripped.startswith("["):
            current = None  # entered some other section
            continue
        if current is None:
            continue
        kv = _KV_RE.match(stripped)
        if kv:
            sections[current][kv.group(1)] = _parse_toml_value(kv.group(2))
    return sections


def _codex_list() -> list[McpServerEntry]:
    out: list[McpServerEntry] = []
    for name, body in _codex_read_sections().items():
        env = body.get("env", {})
        env_keys: list[str] = [str(k) for k in env.keys()] if isinstance(env, dict) else []
        cmd = body.get("command")
        args = body.get("args", [])
        if not isinstance(args, list):
            args = []
        url = body.get("url")
        srv_type = "http" if url else "stdio"
        out.append(
            McpServerEntry(
                name=name,
                scope="user",
                type=srv_type,
                command=cmd if isinstance(cmd, str) else None,
                args=[str(a) for a in args],
                url=url if isinstance(url, str) else None,
                env_keys=env_keys,
            )
        )
    return out


def _toml_quote(s: str) -> str:
    return json.dumps(s)


def _codex_render_section(spec: McpServerSpec) -> str:
    lines = [f"[mcp_servers.{spec.name}]"]
    if spec.command:
        lines.append(f"command = {_toml_quote(spec.command)}")
    if spec.args:
        rendered_args = ", ".join(_toml_quote(a) for a in spec.args)
        lines.append(f"args = [{rendered_args}]")
    if spec.url:
        lines.append(f"url = {_toml_quote(spec.url)}")
    if spec.env:
        rendered_env = ", ".join(f"{k} = {_toml_quote(v)}" for k, v in spec.env.items())
        lines.append(f"env = {{ {rendered_env} }}")
    return "\n".join(lines) + "\n"


def _codex_add(spec: McpServerSpec) -> None:
    section = _codex_render_section(spec)
    if CODEX_CONFIG_PATH.exists():
        existing = CODEX_CONFIG_PATH.read_text()
        sep = "" if existing.endswith("\n\n") else ("\n" if existing.endswith("\n") else "\n\n")
        _atomic_write_text(CODEX_CONFIG_PATH, existing + sep + section)
    else:
        _atomic_write_text(CODEX_CONFIG_PATH, section)


def _codex_remove(name: str) -> None:
    if not CODEX_CONFIG_PATH.exists():
        raise ServerNotFoundError(f"server {name!r} not configured")
    lines = CODEX_CONFIG_PATH.read_text().splitlines(keepends=True)
    out: list[str] = []
    skipping = False
    target = f"[mcp_servers.{name}]"
    for line in lines:
        stripped = line.strip()
        if stripped == target:
            skipping = True
            continue
        if skipping and stripped.startswith("[") and stripped.endswith("]"):
            skipping = False
        if not skipping:
            out.append(line)
    _atomic_write_text(CODEX_CONFIG_PATH, "".join(out))


# ---------------------------------------------------------------------------
# gemini: edit ~/.gemini/settings.json mcpServers block
# ---------------------------------------------------------------------------

GEMINI_SETTINGS_PATH = Path.home() / ".gemini" / "settings.json"


def _gemini_load() -> dict[str, Any]:
    if not GEMINI_SETTINGS_PATH.exists():
        return {}
    try:
        return json.loads(GEMINI_SETTINGS_PATH.read_text())
    except (json.JSONDecodeError, OSError) as e:
        raise BackendError(f"failed to read gemini settings: {e}") from e


def _gemini_save(data: dict[str, Any]) -> None:
    _atomic_write_text(GEMINI_SETTINGS_PATH, json.dumps(data, indent=2))


def _gemini_list() -> list[McpServerEntry]:
    data = _gemini_load()
    servers = data.get("mcpServers", {})
    if not isinstance(servers, dict):
        return []
    out: list[McpServerEntry] = []
    for name, body in servers.items():
        if not isinstance(body, dict):
            continue
        env = body.get("env", {})
        env_keys: list[str] = [str(k) for k in env.keys()] if isinstance(env, dict) else []
        url = body.get("url") or body.get("httpUrl")
        srv_type = "http" if url else "stdio"
        out.append(
            McpServerEntry(
                name=name,
                scope="user",
                type=srv_type,
                command=body.get("command") if isinstance(body.get("command"), str) else None,
                args=list(body.get("args", []) or []),
                url=url if isinstance(url, str) else None,
                env_keys=env_keys,
            )
        )
    return out


def _gemini_add(spec: McpServerSpec) -> None:
    data = _gemini_load()
    servers = data.get("mcpServers", {})
    if not isinstance(servers, dict):
        raise BackendError("gemini settings mcpServers must be an object")
    data["mcpServers"] = servers
    entry: dict[str, Any] = {}
    if spec.command:
        entry["command"] = spec.command
    if spec.args:
        entry["args"] = list(spec.args)
    if spec.url:
        entry["url"] = spec.url
    if spec.env:
        entry["env"] = dict(spec.env)
    servers[spec.name] = entry
    _gemini_save(data)


def _gemini_remove(name: str) -> None:
    data = _gemini_load()
    servers = data.get("mcpServers", {})
    if not isinstance(servers, dict) or name not in servers:
        raise ServerNotFoundError(f"server {name!r} not configured")
    del servers[name]
    _gemini_save(data)
