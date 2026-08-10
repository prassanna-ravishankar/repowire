# Install

Repowire runs on macOS and Linux with tmux. The CLI, daemon, hooks, MCP shim,
Telegram/Slack peers, and hosted relay are native Go binaries. Python is not
required.

## Recommended

```bash
curl -fsSL https://github.com/prassanna-ravishankar/repowire/releases/latest/download/install.sh | sh
```

The installer detects the current OS and architecture, downloads the matching
GitHub Release archive, verifies its SHA-256 checksum, and then runs [setup](setup.md).

Set `REPOWIRE_VERSION=v0.X.Y`, `REPOWIRE_INSTALL_DIR`, or `REPOWIRE_BIN_DIR`
before running the installer to pin a version or choose alternate locations.

## What gets installed

The release archive ships:

- The native `repowire` CLI and daemon (HTTP + WebSocket on `127.0.0.1:8377`).
- The daemon-owned Streamable HTTP MCP implementation and per-agent stdio identity shim.
- Native hook/ws-hook handlers for every detected agent runtime.
- Embedded OpenCode, Pi, Claude channel, and orchestrator workspace assets.
- The Next.js dashboard, pre-built and served from the daemon at `/dashboard`.

The installer runs [setup](setup.md) after placing the files, wiring hooks and
starting the local daemon.

Repowire does not install third-party agent skills. If you want reusable `SKILL.md` packages, use a skills installer such as [Vercel Labs `skills`](https://github.com/vercel-labs/skills) alongside Repowire.
