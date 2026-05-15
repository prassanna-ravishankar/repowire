# CLI

The `repowire` command is a thin wrapper around setup, the daemon, and the bot peers. Most users only ever need `setup`. Everything else is for operators running their own daemon or control surfaces.

## `repowire setup`

```bash
repowire setup [--relay] [--experimental-channels] [--non-interactive]
```

One-time install. Detects every agent runtime present (Claude Code, Codex, Gemini CLI, OpenCode), wires lifecycle hooks and the MCP server for each, and installs the daemon as a user service.

- `--relay` opts in to the hosted relay at `repowire.io`.
- `--experimental-channels` enables the MCP channel transport for Claude Code (v2.1.80+, claude.ai login, bun).
- `--non-interactive` skips prompts and uses flag values only.

## `repowire serve`

```bash
repowire serve [--host HOST] [--port PORT] [--relay]
```

Run the daemon in the foreground. Useful for debugging hooks or running outside the installed service. Defaults to `127.0.0.1:8377`.

## `repowire status`

```bash
repowire status
```

Show what's installed, which agents were detected, and whether the daemon is running.

## `repowire doctor`

```bash
repowire doctor
```

Run a battery of diagnostic checks and print color-coded results. Each check reports `✓` (ok), `⚠` (warn, non-fatal), `✗` (fail), or `·` (skip, not applicable).

Checks include:

- Daemon reachable (`GET /health`, prints version)
- Per-runtime hook + MCP install state (claude-code, codex, gemini, opencode, pi)
- `tmux`, Python, and package-manager (`uv`/`pipx`/`pip`) availability
- Spawn allowlist resolves (commands on `PATH`, paths exist as directories)
- WebSocket auth token state
- Relay reachable (when `relay.enabled`)
- Channel transport (when configured via `--experimental-channels`)

Exits 0 if all checks pass (or only warn/skip). Exits 1 if any check fails — suitable for `bash`-style health gates.

## `repowire peer`

```bash
repowire peer new PATH [--circle CIRCLE]   # spawn a new peer in tmux
repowire peer list                          # god-view list (all circles, includes caller)
repowire peer describe NAME_OR_ID [--circle C]  # full state for one peer
repowire peer prune                         # remove offline peers from the registry
```

`peer list` is god-view: it returns every peer regardless of circle and includes the calling shell. The MCP [`list_peers`](mcp-tools.md#list_peers) tool defaults to a peer-facing view (online only, caller hidden).

`peer describe` accepts either a display name (`clitcoin-claude-code`) or a peer id (`repow-5-abd4d21e`). Pass `--circle` when a display name is ambiguous across circles — without it, the command refuses to guess and prints the same misroute-style refusal the daemon emits internally. Output includes identity (project, circle, role, backend), liveness (status, path, machine, last-seen), open ask threads in both directions, and the last few communication events involving the peer. Reads `GET /peers`, `GET /peers/{id}`, `GET /asks/pending?direction=both`, and `GET /events` — no new daemon endpoints.

## `repowire build-ui`

```bash
repowire build-ui
```

Build the Next.js dashboard into the static export served by the daemon at `/dashboard`. Run after editing files under `web/`.

## `repowire telegram start`

```bash
repowire telegram start
```

Run the Telegram bot peer. Reads `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` from the environment or `~/.repowire/config.yaml`. The bot registers as the `telegram` peer; messages from it are framed as human input.

## `repowire slack start`

```bash
repowire slack start
```

Run the Slack bot peer over Socket Mode (no public URL needed). Reads `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, and `SLACK_CHANNEL_ID` from the environment or config.

## `repowire update`

```bash
repowire update
```

Re-install repowire via the same package manager that installed it. Use after pulling a new release.

## `repowire uninstall`

```bash
repowire uninstall [--yes]
```

Remove hooks, MCP entries, and the daemon service. Prompts before deleting `~/.repowire/` (config, logs, attachments); decline to keep it for reinstalls. `--yes` skips the prompts and removes the directory along with the installed package.

## See also

- Configuration lives in `~/.repowire/config.yaml`. See [configuration](configuration.md).
- The [MCP tools](mcp-tools.md) reference covers what agents call once the daemon is running.
