# CLI

The native Go `repowire` command owns setup, daemon operation, hooks, mesh
control, Telegram and Slack peers, and the hosted relay server.

## `repowire setup`

```bash
repowire setup [--relay] [--experimental-channels] [--http-mcp] [--update-checks|--no-update-checks] [--no-service] [--non-interactive]
```

One-time install. Detects Claude Code, Codex, Gemini CLI, Antigravity, OpenCode,
and Pi; wires each available transport; configures `/mcp` plus the stdio identity
shim; and installs the Go daemon as a user service.

Setup uses the SQLite daemon state store. The daemon applies idempotent
migrations on startup, imports legacy `schedules.json`, `events.json`, and
`sessions.json` once, and leaves those JSON files in place for downgrade/export
compatibility. Migrated state is written to `state.db`, not back to those JSON
files.

- `--relay` opts in to the hosted relay at `repowire.io`.
- `--experimental-channels` enables the experimental MCP channel / ACP transport for Claude Code (v2.1.80+, claude.ai login, bun).
- `--http-mcp` is accepted for older setup scripts. Normal setup already enables the localhost `/mcp` implementation and generates `daemon.auth_token` if needed.
- `--update-checks` lets `repowire status` and `repowire doctor` check GitHub Releases for newer Repowire versions and suggest `repowire update`. Checks are disabled by default and updates are never auto-applied. Use `--no-update-checks` to turn the check off again.
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

Runs the same daemon/runtime/integration report as `status`, then checks the
host prerequisites used by the core (`tmux` and `git`). It exits non-zero when
the daemon is unavailable; missing optional host tools are reported as warnings.

## `repowire peer`

```bash
repowire peer new PATH [--backend BACKEND] [--profile PROFILE] [--circle CIRCLE]
repowire peer list                          # god-view list (all circles, includes caller)
repowire peer describe NAME_OR_ID [--circle C]  # daemon state for one peer
repowire peer ask NAME QUERY [--timeout SEC] [--circle C]  # blocking compatibility ask
repowire peer claim-role orchestrator [--peer NAME_OR_ID] [--circle C] [--force]
repowire peer restart NAME_OR_ID [--circle C] [--dry-run] [-m MESSAGE]
repowire peer doctor NAME_OR_ID [--circle C] [--json] [--fix]  # deep diagnostic + contradictions
repowire peer rehook NAME_OR_ID [--circle C] [--apply]         # re-establish inbound ws-hook (non-destructive)
repowire peer prune [--dry-run] [--force]   # confirm, then remove offline peers
repowire peer whoami [--register --backend B --name NAME --circle C --path P]  # read-only identity, or self-register
repowire peer asks [--peer-id ID | --pane-id PANE | --peer NAME] [--direction inbound|outbound|both] [--json]
repowire peer deliveries [--peer-id ID | --pane-id PANE | --peer NAME] [--json]
repowire peer ack CORR_ID [-m MESSAGE] [--from-peer NAME]
```

`peer whoami`, `peer asks`, `peer deliveries`, and `peer ack` are the shellable mesh primitives intended for agents whose hooks don't fire today (notably [Antigravity `agy`](../use/features/connect-antigravity.md)). They wrap daemon HTTP endpoints (`/peers`, `/peers/by-pane`, `/asks/pending`, `/deliveries/pending`, `/ack`) and automatically use the local `daemon.auth_token` when configured. Identity resolves in this order: explicit `--peer-id` → `--pane-id` → `$TMUX_PANE` → `--peer NAME`. Use `peer whoami --register --backend antigravity` once at session start to self-onboard; it uses the configured tmux session/window boundary or requires `--circle` outside tmux.

`peer deliveries` drains one-shot queued deliveries for a peer. Draining deletes the queued rows to avoid duplicate paste/replay. For queued asks, `peer deliveries` shows the original ask text once, while `peer asks` continues to show the open ask until the agent closes it with `peer ack` or the MCP `ack` tool.

For Antigravity interop checks, use `peer whoami`, `peer deliveries`, `peer asks`, and `peer ack` to exercise the CLI fallback directly against the live daemon.

`peer list` is god-view: it returns every peer regardless of circle and includes the calling shell. The MCP [`list_peers`](mcp-tools.md#list_peers) tool defaults to a peer-facing view (online only, caller hidden).

`peer ask` is a blocking CLI compatibility helper for quick manual checks. It uses the daemon's ask/answer lifecycle under the hood, waits for the recipient to `ack` with a reply, then prints the reply text. For agent-to-agent work, prefer the MCP [`ask`](mcp-tools.md#ask) tool, which returns a correlation id immediately and lets the conversation continue asynchronously.

`peer new` spawns a tmux-backed peer through the daemon `/spawn` route using the configured `daemon.spawn.commands.<backend>` command. Inside tmux it discovers the current session or window according to `daemon.circle_boundary`; window mode also forwards the current pane internally so the daemon can place the new peer in that window. There is no separate window flag. Outside tmux, pass an explicit circle in session mode; window mode requires tmux window evidence. Pass `--profile NAME` to append args from `daemon.spawn.profiles.<backend>.<name>`, such as a faster or more capable model selection. Antigravity uses the same daemon pre-registration path as MCP spawn, so it appears immediately as a CLI-fallback peer while upstream hooks are pending. `--command` remains accepted as a deprecated explicit override and bypasses daemon registration/profile resolution.

`peer describe` accepts either a display name (`clitcoin-claude-code`) or a peer
id (`repow-5-abd4d21e`). Pass `--circle` when a display name is ambiguous across
circles; the daemon refuses to guess. Output is the canonical `GET
/peers/{identifier}` state, including identity, liveness, inbound health, and
runtime metadata.

`peer claim-role orchestrator` repairs an existing registered peer when the durable session mapping has the wrong role after daemon restart. It updates the live peer and its persisted session mapping, demoting offline or stale orchestrator holders in the same circle. It refuses to demote a fresh online/busy holder, even with `--force`; stop the current holder first if you intentionally need to replace it. Omit `--peer` only from inside a registered peer shell where Repowire can discover the current peer.

`peer restart` restarts a peer on the same backend, path, circle, role, and mesh identity by using the backend's native resume command. It is for continuing the same backend conversation without changing the address other peers use. The daemon only preserves the same `peer_id` through the normal exact backend+path reconnect path; it does not force-rebind arbitrary peer IDs. Restart is same-host and strict: Repowire must first validate a captured runtime session id against local backend session storage. If no valid resume is available, restart refuses before killing anything.

Restart is same-window/name first, not same-pane. Killing a tmux pane destroys that pane, so this slice respawns through the normal Repowire spawn path and lets tmux allocate a fresh pane/window name using the existing naming rules. The response may include the new `tmux_session`, but it does not promise the same pane id.

If the peer has a captured runtime session id (from a session binding or hook metadata) and a matching local session is found on disk, restart relaunches with the backend's native resume command in the original working directory and reports `resume_mode=resumed`. Otherwise it returns `409 resume_unavailable` with a `resume_warning` explaining why (no captured id, backend doesn't support resume, the id has no local session file — stale/expired, or the backend's session store is not yet mappable for pre-validation).

The id is **pre-validated against on-disk session storage before the pane is killed**, because resume-capable backends exit non-zero on an unknown id rather than starting fresh; resuming a stale id after killing the pane would leave the peer dead. Per-backend storage that Repowire validates against:

| Backend | Resume command | Session store validated |
|---------|----------------|-------------------------|
| claude-code | `claude --resume <id>` | `~/.claude/projects/<cwd>/<id>.jsonl` |
| codex | `codex resume <id>` | `~/.codex/sessions/**/rollout-*<id>.jsonl` |
| opencode | `opencode --session <id>` | `~/.local/share/opencode/storage/session/*/ses_<id>.json` (id + directory) |
| gemini | `gemini --resume <id>` | `~/.gemini/tmp/<hash>/chats/session-*.json` (sessionId) |
| antigravity | `agy --conversation <id>` | `~/.gemini/antigravity-cli` (last_conversations + `<id>.pb`; validates the last conversation per cwd) |
| pi | `pi --session <id>` | `~/.pi/pi-acp/session-map.json` (cwd + sessionFile) |

This same resume-safety check is shared by restart, durable/recurring jobs, and session control, so a stale id never resumes anywhere. Resume does not persist the originally selected spawn profile.

When a live pane must be stopped, destructive proof is required. Repowire-spawned panes use durable spawn ownership. Manually-created panes are killable only when live pane hook metadata contains the target `peer_id`; cwd/path match alone is not enough because multiple peers commonly share one repository. If the peer is offline and no live pane exists, restart skips the kill and spawns the validated resume command.

`peer doctor` is the operator-facing counterpart to automatic lazy repair. It runs lazy repair first, re-resolves the peer, then reports registry identity, inbound reachability (WebSocket connection, tmux pane existence, pane hook metadata, agent pid liveness), pending ask state, and any detected contradictions. Contradiction codes: `ONLINE_BUT_NO_WS`, `PANE_MISSING`, `AGENT_PID_DEAD`, `HOOK_PEERID_MISMATCH`, `WS_PANE_MISMATCH` (warning), `STALE_PENDING_ASK` (warning). Local-only probes (tmux, hook metadata, pid) degrade to `unavailable` for peers on a different machine than the daemon. The command exits non-zero when any error-severity contradiction is present, so it is usable as a health gate. `--json` emits the raw report. `--fix` attempts a non-destructive `peer rehook --apply` when an inbound-down contradiction (`ONLINE_BUT_NO_WS` / `PANE_MISSING`) is found.

`peer rehook` re-establishes a peer's inbound ws-hook **without killing the pane or the agent** — the non-destructive recovery for the "registered/online but inbound delivery is dead" case. It is same-host only (the daemon cannot spawn a ws-hook into a foreign pane) and gated by spawn-ownership proof or live tmux pane evidence. It pings an existing connection first and never disconnects a ping-healthy peer (reports `already_healthy`). It defaults to a dry-run report; pass `--apply` to act. If the pane hook metadata names a stale peer id/display name (`HOOK_PEERID_MISMATCH`), rehook rewrites that metadata to the registry identity, drops the stale birth certificate, starts a fresh ws-hook, and reports `reconciled_hook_metadata`. In the non-mismatch respawn path, when the prior ws-hook process is still alive by pid even though its WebSocket is dead, the underlying respawn is a no-op and the response reports `respawn_skipped_pid_alive_or_contested`. For a destructive restart use `peer restart`.

## `repowire session`

```bash
repowire session resume REPOWIRE_SESSION_ID [--dry-run] [--profile PROFILE] [-m MESSAGE] [--json]
```

`session resume` starts a detached durable Repowire session using the backend's
native resume command. By default it acts; pass `--dry-run` to only validate
that the captured runtime session id is safe to resume. It uses the same
resume-safety check as `peer restart` and jobs, so active sessions, missing
runtime ids, unsupported backends, and stale ids whose local session file is
gone fail loudly with a non-zero exit instead of starting fresh.

## `repowire trace`

```bash
repowire trace TRACE_ID [--json]
```

Shows the recorded delivery stages for one message — an ask (use its `correlation_id`) or a notify (use its `delivery_id`, returned in the `/notify` response). Stages are ordered (`created → resolved_peer → routed → websocket_sent → hook_received → pane_injected → … → acked → closed`, plus failure stages `resolve_failed`, `no_connection`, `injection_failed`). Terminal stages are recorded **truthfully** from the transport outcome: `pane_injected` only when the ws-hook returned an `injected` delivery receipt; `injection_failed` on a `failed`/`rejected` receipt; and `websocket_sent` (unverified) for ACP delivery or legacy hooks that don't acknowledge, rather than assuming injection. This reads the local delivery trace ledger (`GET /traces/{trace_id}`); no external tracing infrastructure is required, and rows older than `daemon.prune_max_age_hours` are pruned during lazy repair. Currently covers ask and notify; query/broadcast pane stages are not yet traced. Exits non-zero if any stage failed.

## `repowire share`

```bash
repowire share PEER_NAME [--rw] [--ttl SECS]
repowire share --list
repowire share --revoke SHARE_ID
```

Generate a shareable browser link for a running peer. Requires relay (`repowire
setup --relay`). The link is served by the relay and requires no Repowire
installation to open.

| Flag | Description |
|---|---|
| `PEER_NAME` | Display name of the peer to share. Required — use `repowire peer list` to see registered peers. |
| `--rw` | Read-write link; viewer can inject asks. Default is read-only. |
| `--ttl SECS` | Link lifetime in seconds (> 0). Default: no expiry. |
| `--list` | Show all active share links for this daemon. |
| `--revoke SHARE_ID` | Revoke a share link immediately. |

The command prints the share URL and the `share_id` needed for revocation.

Links are scoped to one peer. The relay invalidates all links when it restarts.

## `repowire schedule`

```bash
repowire schedule self WHEN_OR_CRON TEXT [--cron] [--kind notify|ask] [--circle CIRCLE]
repowire schedule create TO_PEER WHEN_OR_CRON TEXT --from-peer FROM_PEER [--cron] [--kind notify|ask] [--circle CIRCLE]
repowire schedule list [--from-peer FROM_PEER]
repowire schedule delete SCHEDULE_ID
```

Create one-shot or recurring scheduled mesh messages. Without `--cron`, `WHEN_OR_CRON` may be ISO-8601 or a relative time like `10m`, `1h`, or `in 30s`. With `--cron`, it is a five-field cron expression or alias such as `@hourly`, `@daily`, `@midnight`, `@weekly`, or `@monthly`.

`schedule self` targets the current CLI peer identity by default and is the easiest way to wake the same session later. `schedule create` targets another peer and requires `--from-peer` so the daemon knows who the scheduled message is from. `--kind ask` opens an ask thread when the schedule fires; `notify` is fire-and-forget.

## `repowire jobs`

```bash
repowire jobs create TITLE [--kind KIND] [--prompt TEXT | --prompt-file PATH] [--assigned-peer PEER] [--path PATH --backend BACKEND] [--profile NAME] [--due-at TIME | --cron EXPR] [--process-scope per-fire|persistent] [--continuity resume|fresh] [--result-surface NAME] [--json]
repowire jobs run JOB_ID [--json]
repowire jobs retry JOB_ID [--json]
repowire jobs list [--state STATE] [--owner PEER_ID] [--created-by PEER_ID] [--session SESSION_ID] [--circle CIRCLE] [--json]
repowire jobs show JOB_ID [--json]
repowire jobs update JOB_ID --state STATE [--attempt-id ATTEMPT_ID] [--reason REASON] [--phase PHASE] [--note NOTE] [--result-summary TEXT] [--json]
repowire jobs cancel JOB_ID [--requested-by PEER_ID] [--reason REASON] [--json]
repowire jobs result JOB_ID [--json]
```

Create, run, retry, and inspect daemon-owned tracked work records. Jobs are
durable control state in `state.db`; they can exist without a live peer, ask
thread, schedule, or session. Creating a one-shot job persists an execution
spec but does not dispatch it until the daemon runner reaches `due_at` or you
call `repowire jobs run JOB_ID`.

Pass `--cron` instead of `--due-at` to create a recurring durable job template.
`--due-at` accepts the same one-shot time forms as `repowire schedule`
(ISO-8601 or compact relatives such as `10m`, `1h`, or `in 30s`). `--cron`
uses the same five-field cron syntax and aliases as schedules (`@daily`,
`@hourly`, and so on). The returned id starts with `cal-`. The daemon
materializes each due calendar template into a normal child `work-` job and
then dispatches that child through the same runner. Missed recurring fires
coalesce into one child occurrence before the next future fire; Repowire does
not backfill a flood of missed runs. Cancel a `cal-` id to stop future child
creation. Existing child jobs are not cancelled automatically. Recurring Codex
jobs preserve the latest observed runtime binding for the same calendar/path
and can launch `codex resume <runtime-session-id>` when compatible resume
metadata exists; otherwise the runner falls back to normal spawn behavior.
Unassigned path/backend jobs default to `process_scope=per_fire`: each run uses
a daemon-spawned or backend-resumed executor process, and terminal completion
releases that process. Reused persistent executors and explicitly assigned peers
are left alive.
One-shot jobs default to `continuity=fresh`; recurring jobs default to
`continuity=resume` so the next fire uses the backend-native runtime session id
when available. Pass `--continuity fresh` on recurring jobs to start each fire
without backend-native resume, or `--process-scope persistent` for the older
live-peer reuse behavior.

Use `--assigned-peer` for an exact peer id/name. Ambiguous display names are
rejected before persistence. Without an assigned peer, pass `--path` and
`--backend` (plus optional `--profile`) so the daemon can reuse a live matching
worker, resume a recorded Codex runtime when available, or spawn a worker
through the same guardrails as `/spawn`. Delivery is an `ask`; ack is only
receipt, and workers should first mark receipt/start with the current attempt
id, for example `repowire jobs update JOB_ID --state running --attempt-id
ATTEMPT_ID`, then complete with a terminal update using the same attempt id.

Agent folders are a convention, not a registry. For standing workers such as a
daily brief, create a folder with runtime instructions (`AGENTS.md` for Codex,
`CLAUDE.md` for Claude Code, or both via symlink/copy), then target it with
`--path <folder> --backend <runtime>`. `--result-surface` is metadata only in
this slice; the daemon does not automatically send email, Telegram, or
dashboard notifications from it.

Jobs commands are script-safe: daemon connection failures, missing jobs, and
HTTP errors exit non-zero. Pass `--json` when scripts need the status, list, or
result payload.

## `repowire agents`

```bash
repowire agents create NAME [--path PATH] [--backend BACKEND] [--force] [--json]
```

Scaffold a local worker folder for durable jobs. By default, the folder is
created at `.repowire/agents/<name>` under the current Git repo root, or under
the current directory when outside a Git repo. The command creates `AGENTS.md`
as the source of truth, `CLAUDE.md` as a relative symlink to `AGENTS.md`, and a
small `README.md` with a suggested `repowire jobs create ... --path ...`
command.

Agent folders are a convention, not a registry: jobs still target them with
`--path` and `--backend`. `--backend` on `agents create` only fills the
suggested jobs command. The scaffold path is absolute so daemon spawn does not
depend on the daemon's current directory.

## `repowire orchestrator`

```bash
repowire orchestrator init [--force]
repowire orchestrator diff
repowire orchestrator start [--runtime RUNTIME] [--profile PROFILE] [--circle CIRCLE]
```

`init` renders the orchestrator workspace and skill pack embedded in the Go
binary; `--force` first moves the current workspace to a timestamped backup.
`diff` reports Repowire-owned template files that differ or are missing.
`start` selects an installed runtime (Pi first when its Repowire extension is
installed), uses `--circle`, the configured circle, or the current tmux
session/window boundary in that order, and spawns with `role=orchestrator`. Outside tmux, a circle must
be chosen explicitly or configured; Repowire never invents one.

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
then points `~/.repowire/orchestrator/SOUL.md` at the resolved persona file.
The orchestrator template references that stable shim with `@SOUL.md`. Persona
context is identity guidance, not a permission policy.

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

Approval is a product contract, not extra CLI magic in this slice: proposed
memory writes must show a user-visible full-file or unified diff before someone
runs `repowire memory write`. Rejections leave files unchanged; edited
proposals need a fresh diff before write.

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

Download and install the latest checksum-verified native release, then re-run
non-interactive setup. SQLite state migrations run when the daemon restarts;
verify with `repowire doctor`.

`repowire update` is the only command that upgrades the installed binary.
Hooks, MCP calls, daemon routing, `status`, and `doctor` never auto-update
Repowire.

## `repowire uninstall`

```bash
repowire uninstall [--yes]
```

Remove hooks, MCP entries, and the daemon service. `--yes` also removes `~/.repowire/` (config, logs, attachments); omit it to keep local state for reinstalls. Remove the release binary separately if desired.

## See also

- Configuration lives in `~/.repowire/config.yaml`. See [configuration](configuration.md).
- The [MCP tools](mcp-tools.md) reference covers what agents call once the daemon is running.
