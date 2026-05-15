# Install

Repowire runs on macOS and Linux with Python 3.10+ and tmux.

## Recommended: uv

```bash
uv tool install repowire
```

Fast, isolated, and the way the project itself is developed.

## Alternatives

```bash
pipx install repowire
pip install repowire
```

Use `pipx` if you want isolation without uv. Use plain `pip` only inside a virtualenv you control.

## Interactive installer

```bash
curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh
```

The installer detects `uv`, `pipx`, and `pip` in that order and falls through to whichever it finds. It then drops you into [setup](setup.md).

## What gets installed

The package ships:

- The `repowire` CLI (`repowire setup`, `repowire serve`, `repowire telegram start`, …).
- The local daemon (HTTP + WebSocket on `127.0.0.1:8377`).
- The MCP server (stdio).
- Hook scripts for every agent runtime the setup step detects.
- The Next.js dashboard, pre-built and served from the daemon at `/dashboard`.

Nothing runs yet. Run [setup](setup.md) to wire the hooks for your agents.
