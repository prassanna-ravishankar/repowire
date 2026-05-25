# CLI

The `repowire` command is a thin wrapper around setup, the daemon, and the bot peers. Most users only ever need `setup`. Everything else is for operators running their own daemon or control surfaces.

## `repowire setup`

```bash
repowire setup [--relay] [--experimental-channels] [--http-mcp] [--update-checks|--no-update-checks] [--no-service] [--non-interactive]
```

One-time install. Detects every supported agent runtime present (Claude Code, Codex, Gemini CLI, OpenCode), wires the appropriate Repowire transport for each, and installs the daemon as a user service.

Setup uses the SQLite daemon state store. The daemon applies idempotent
migrations on startup, imports legacy `schedules.json`, `events.json`, and
`sessions.json` once, and leaves those JSON files in place for downgrade/export
compatibility. Migrated state is written to `state.db`, not back to those JSON
files.

- `--relay` opts in to the hosted relay at `repowire.io`.
- `--experimental-channels` enables the experimental MCP channel / ACP transport for Claude Code (v2.1.80+, claude.ai login, bun).
- `--http-mcp` enables the experimental localhost Streamable HTTP MCP endpoint at `http://127.0.0.1:8377/mcp` and generates `daemon.auth_token` if needed.
- `--update-checks` lets `repowire status` and `repowire doctor` check PyPI for newer Repowire releases and suggest `repowire update`. Checks are disabled by default and updates are never auto-applied. Use `--no-update-checks` to turn the check off again.
- `--no-service` skips daemon service installation; run `repowire serve` manually.
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

When `updates.check_enabled` is true, status also reports whether a newer Repowire release is available. It does not upgrade anything.

## `repowire service`

```bash
repowire service install
repowire service restart
repowire service status
repowire service uninstall
```

Manage the installed daemon user service. `install` writes and starts the
platform service (`launchd` on macOS, `systemd --user` on Linux), `restart`
restarts the installed daemon after a local reinstall or config change,
`status` shows whether it is installed/running, and `uninstall` removes the
service entry. Prefer these commands over raw `launchctl` or `systemctl`
unless you are troubleshooting the platform service manager directly.

## `repowire doctor`

```bash
repowire doctor
```

Run a battery of diagnostic checks and print color-coded results. Each check reports `✓` (ok), `⚠` (warn, non-fatal), `✗` (fail), or `·` (skip, not applicable).

Checks include:

- Daemon reachable (`GET /health`, prints version)
- Per-runtime hook + MCP install state (claude-code, codex, gemini, antigravity, opencode, pi)
- `tmux`, Python, and package-manager (`uv`/`pipx`/`pip`) availability
- Update availability when opted in with `repowire setup --update-checks`
- Spawn allowlist resolves (commands on `PATH`, paths exist as directories)
- WebSocket auth token state
- SQLite state database integrity, schema version, import audit, event count, and peer mapping count
- Relay reachable (when `relay.enabled`)
- Channel transport (when configured via `--experimental-channels`)

Exits 0 if all checks pass (or only warn/skip). Exits 1 if any check fails — suitable for `bash`-style health gates.

## `repowire peer`

```bash
repowire peer new PATH [--backend BACKEND] [--profile PROFILE] [--circle CIRCLE]
repowire peer list                          # god-view list (all circles, includes caller)
repowire peer describe NAME_OR_ID [--circle C]  # full state for one peer
repowire peer claim-role orchestrator [--peer NAME_OR_ID] [--circle C] [--force]
repowire peer restart NAME_OR_ID [--circle C] [--dry-run] [-m MESSAGE]
repowire peer prune                         # remove offline peers from the registry
repowire peer whoami [--register --backend B --name NAME --circle C --path P]  # read-only identity, or self-register
repowire peer asks [--peer-id ID | --pane-id PANE | --peer NAME] [--direction inbound|outbound|both] [--json]
repowire peer deliveries [--peer-id ID | --pane-id PANE | --peer NAME] [--json]
repowire peer ack CORR_ID [-m MESSAGE] [--from-peer NAME]
```

`peer whoami`, `peer asks`, `peer deliveries`, and `peer ack` are the shellable mesh primitives intended for agents whose hooks don't fire today (notably [Antigravity `agy`](../agents/antigravity.md)). They wrap daemon HTTP endpoints (`/peers`, `/peers/by-pane`, `/asks/pending`, `/deliveries/pending`, `/ack`) and automatically use the local `daemon.auth_token` when configured. Identity resolves in this order: explicit `--peer-id` → `--pane-id` → `$TMUX_PANE` → `--peer NAME`. Use `peer whoami --register --backend antigravity` once at session start to self-onboard.

`peer deliveries` drains one-shot queued deliveries for a peer. Draining deletes the queued rows to avoid duplicate paste/replay. For queued asks, `peer deliveries` shows the original ask text once, while `peer asks` continues to show the open ask until the agent closes it with `peer ack` or the MCP `ack` tool.

For Antigravity interop checks, `python3 scripts/agy_interop_smoke.py --run-cli-fallback` writes a JSON evidence report covering observed hook evidence, MCP availability state, and CLI fallback ask→ack. It records current behaviour only; hook or MCP support is not treated as verified unless the report observes matching daemon evidence.

`peer list` is god-view: it returns every peer regardless of circle and includes the calling shell. The MCP [`list_peers`](mcp-tools.md#list_peers) tool defaults to a peer-facing view (online only, caller hidden).

`peer new` spawns a tmux-backed peer using the configured `daemon.spawn.commands.<backend>` command. Pass `--profile NAME` to append args from `daemon.spawn.profiles.<backend>.<name>`, such as a faster or more capable model selection. `--command` remains accepted as a deprecated explicit override and bypasses profile resolution.

`peer describe` accepts either a display name (`clitcoin-claude-code`) or a peer id (`repow-5-abd4d21e`). Pass `--circle` when a display name is ambiguous across circles — without it, the command refuses to guess and prints the same misroute-style refusal the daemon emits internally. Output includes identity (project, circle, role, backend), liveness (status, path, machine, last-seen), open ask threads in both directions, and the last few communication events involving the peer. Reads `GET /peers`, `GET /peers/{id}`, `GET /asks/pending?direction=both`, and `GET /events` — no new daemon endpoints.

`peer claim-role orchestrator` repairs an existing registered peer when the durable session mapping has the wrong role after daemon restart. It updates the live peer and its persisted session mapping, demoting offline or stale orchestrator holders in the same circle. It refuses to demote a fresh online/busy holder unless `--force` is passed. Omit `--peer` only from inside a registered peer shell where Repowire can discover the current peer.

`peer restart` intentionally restarts a daemon-spawned peer on the same backend, path, circle, role, and mesh identity. It is for reloading runtime startup context such as `AGENTS.md` or an orchestrator `SOUL.md` without changing the address other peers use. The daemon only preserves the same `peer_id` through the normal exact backend+path reconnect path; it does not force-rebind arbitrary peer IDs. Restart is same-host and requires explicit Repowire spawn ownership proof plus live tmux evidence for the recorded pane. If the peer was attached manually, or if the recorded pane is gone or no longer matches the proof, the daemon cannot prove it is safe to kill and the command refuses instead of touching the pane. Use `--dry-run` to check whether a peer is restartable.

Restart is same-window/name first, not same-pane. Killing a tmux pane destroys that pane, so this slice respawns through the normal Repowire spawn path and lets tmux allocate a fresh pane/window name using the existing naming rules. The response may include the new `tmux_session`, but it does not promise the same pane id.

Restart uses `resume_mode=fresh_runtime_context`. That means the new runtime loads its normal startup context and mesh primer again; it does not guarantee transcript replay or exact backend conversation resume. This slice also does not persist the originally selected spawn profile, so restart uses the current configured backend command. If your configured backend command includes native resume flags, Repowire still reports `fresh_runtime_context` unless Repowire deliberately selected and verified a backend-specific resume mode.

## `repowire schedule`

```bash
repowire schedule self WHEN_OR_CRON TEXT [--cron] [--kind notify|ask] [--circle CIRCLE]
repowire schedule create TO_PEER WHEN_OR_CRON TEXT --from-peer FROM_PEER [--cron] [--kind notify|ask] [--circle CIRCLE]
repowire schedule list [--from-peer FROM_PEER]
repowire schedule delete SCHEDULE_ID
```

Create one-shot or recurring scheduled mesh messages. Without `--cron`, `WHEN_OR_CRON` may be ISO-8601 or a relative time like `10m`, `1h`, or `in 30s`. With `--cron`, it is a five-field cron expression or alias such as `@hourly`, `@daily`, `@midnight`, `@weekly`, or `@monthly`.

`schedule self` targets the current CLI peer identity by default and is the easiest way to wake the same session later. `schedule create` targets another peer and requires `--from-peer` so the daemon knows who the scheduled message is from. `--kind ask` opens an ask thread when the schedule fires; `notify` is fire-and-forget.

## `repowire orchestrator persona`

```bash
repowire orchestrator persona list
repowire orchestrator persona show [NAME]
repowire orchestrator persona path [NAME]
repowire orchestrator persona use NAME
repowire orchestrator persona clear
```

Manage orchestrator persona `SOUL.md` files. Repowire resolves personas from
`~/.repowire/orchestrator/personas/<name>/SOUL.md` first, then
`~/.repowire/personas/<name>/SOUL.md`. `use` writes the workspace active marker,
and the orchestrator `SessionStart` hook injects the active persona with its
source path and SHA-256 short hash. Persona context is identity guidance, not a
permission policy.

## `repowire memory`

```bash
repowire memory path [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory list [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory show SLUG [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory search QUERY [--scope SCOPE] [--project NAME] [--persona NAME] [--all]
repowire memory write SLUG --body BODY [--scope SCOPE] [--project NAME] [--persona NAME] [--type TYPE] [--description TEXT] [--append|--force]
```

Inspect and explicitly write filesystem-backed mesh memory under
`~/.repowire/memory/`. The CLI resolves scope directories, lists Markdown memory
files, prints a memory by slug, searches memory text, and writes one curated
memory at a time. It never auto-writes from hooks, transcripts, scheduler jobs,
or daemon side effects.

Scopes are `global`, `user`, `project`/`projects`, `persona`/`personas`, and
`orchestrator`. When omitted, the default is `orchestrator` inside the
orchestrator workspace and `user` elsewhere. Project scope defaults to the
current directory name unless `--project NAME` is passed. Persona scope uses the
active orchestrator persona when one is set, or `--persona NAME`.

`write` creates `<slug>.md` with frontmatter (`name`, `description`,
`metadata.type`, `metadata.updated_at`) and refreshes the scope `MEMORY.md`
index. Existing memories are protected by default; pass `--force` to overwrite
or `--append` to add to the current body.

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
When config enables optional runtime support such as ACP, `update` upgrades the
matching package extra so the service runtime keeps the dependency. After
reinstalling hooks/plugins, `update` restarts the daemon service when it is
running. SQLite state migrations run during that daemon restart; verify with
`repowire doctor`.

`repowire update` is the only command that upgrades the installed package.
Hooks, MCP calls, daemon routing, `status`, and `doctor` never auto-update
Repowire.

When config enables an optional runtime surface that requires package extras,
such as `experiments.acp_broker_client`, `update` uses an explicit package spec
like `repowire[acp]` where the package manager supports it so optional
dependencies are preserved.

## `repowire uninstall`

```bash
repowire uninstall [--yes]
```

Remove hooks, MCP entries, and the daemon service. Prompts before deleting `~/.repowire/` (config, logs, attachments); decline to keep it for reinstalls. `--yes` skips the prompts and removes the directory along with the installed package.

## See also

- Configuration lives in `~/.repowire/config.yaml`. See [configuration](configuration.md).
- The [MCP tools](mcp-tools.md) reference covers what agents call once the daemon is running.
