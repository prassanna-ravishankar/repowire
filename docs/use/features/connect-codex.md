# Codex

OpenAI's Codex CLI connects through its native App Server thread API. Repowire
keeps the normal Codex TUI visible; App Server replaces tmux keystroke injection
as the message transport.

## What gets installed

| Surface | Purpose |
| --- | --- |
| `repowire-codex` user service | Runs the local Codex App Server and thread bridge |
| `~/.codex/config.toml` | Installs the Repowire MCP tools |
| `~/.codex/hooks.json` | Repowire Codex hooks are removed when native App Server support is available |

The MCP entry points at the installed `repowire` binary:

```toml
[mcp_servers.repowire]
command = "repowire"
args = ["mcp"]

[mcp_servers.repowire.env]
REPOWIRE_BACKEND = "codex"
```

Older Codex releases without `app-server --listen` retain the hooks transport.

## Registration and delivery

`repowire setup` installs an independently supervised Codex companion. It starts
`codex app-server --listen unix://`; plain `codex`, `codex resume`, and the normal
TUI automatically use that local control socket.

A thread registers as soon as Codex creates it, before its first user prompt.
There is no warmup prompt or `UserPromptSubmit` one-turn delay. Repowire sends an
idle thread a native `turn/start` request and steers an active thread with
`turn/steer`. App Server lifecycle notifications drive `busy` and `online`
status, including interrupt and completion boundaries.

Tmux remains useful for hosting and restarting a TUI, but it is not used for
message delivery. When exactly one tmux circle matches a Codex thread's working
directory, Repowire preserves that session/window circle without binding the
peer to a pane. Spawn hints take precedence. A standalone thread with no safe
placement evidence joins the explicit `default` circle.

The App Server companion is separate from the Repowire daemon. `repowire service
restart` restarts routing without killing Codex threads; `repowire service stop`
or `uninstall` stops both services.

## Verifying

```bash
repowire service status
codex
# in another terminal, before prompting Codex:
repowire peer list
```

The Codex peer should already be listed. Its metadata reports
`transport=codex-app-server`, and its TUI remains interactive.

## Troubleshooting

- Codex peer never registers → run `repowire service status`, then inspect
  `~/.repowire/codex-bridge.log`.
- Codex joins `default` instead of a tmux circle → more than one Codex tmux
  circle matched the same working directory, or none did. Spawn it through
  Repowire for an explicit circle.
- MCP tools return errors → check `~/.codex/config.toml` contains
  `[mcp_servers.repowire]` and that `repowire` is on the service `PATH`.
