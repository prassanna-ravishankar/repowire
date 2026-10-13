# CLI

The native Go `repowire` command owns setup, daemon operation, hooks, mesh
control, Telegram and Slack peers, and the hosted relay server.

Run `repowire --help` to browse commands and `repowire <command> --help` for
command-specific usage and flags. Shell completions are available through
`repowire completion <bash|zsh|fish|powershell>`.

Interactive peer listings adapt to the terminal width. Redirected output keeps
the stable tab-separated format for scripts and pipelines.

## `repowire setup`

```bash
repowire setup [--git-hooks] [--relay] [--experimental-channels] [--http-mcp] [--update-checks|--no-update-checks] [--no-service] [--non-interactive]
```

One-time install. Detects Claude Code, Codex, OpenCode,
and Pi; wires each available transport; configures `/mcp` plus the stdio identity
shim; and installs the Go daemon as a user service.

Setup uses the SQLite daemon state store. The daemon applies idempotent
migrations on startup, imports legacy `schedules.json`, `events.json`, and
`sessions.json` once, and leaves those JSON files in place for downgrade/export
compatibility. Migrated state is written to `state.db`, not back to those JSON
files.

- `--git-hooks` installs only the current repository’s `prepare-commit-msg` hook; it does not configure runtimes or restart services. Run ordinary setup separately. Existing hooks and custom `core.hooksPath` are left unchanged with instructions for manual integration.
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

Show what's installed, which agents were detected, whether Claude's experimental
channel transport is enabled, and whether the daemon is running.

When `updates.check_enabled` is true, status also reports whether a newer Repowire release is available. It does not upgrade anything.

## `repowire service`

```bash
repowire service install
repowire service start
repowire service restart [daemon|bridge|all]
repowire service status
repowire service uninstall
```

Manage the installed user services. `install` writes and starts the daemon and,
when Codex auto-starts its shared App Server, the Repowire Codex bridge.
`start` starts installed services. `restart` defaults to the routing daemon;
`restart bridge` replaces the adapter; and `restart all` replaces both daemon
and adapter. None of them touch Codex's App Server or live Codex threads. `status` reports installed services. `stop` through
`repowire daemon stop` and `uninstall` stop every service. Prefer these commands
over raw `launchctl` or `systemctl` unless troubleshooting the service manager.

Codex owns its App Server: every Codex TUI starts the shared server when none is
running, and `io.repowire.codex-bridge` only attaches to its Unix socket. On
macOS, `service install` removes the `io.repowire.codex-app-server` LaunchAgent
that earlier releases installed and starts Codex's own server so open Codex
sessions reconnect.

Generated service environments keep normal executable search paths but omit
protected user folders (`Desktop`, `Documents`, `Downloads`, `Movies`, `Music`,
and `Pictures`) and ephemeral Codex shim directories. This prevents an unrelated
PATH lookup from causing macOS Files & Folders prompts under a service's name.

## `repowire doctor`

```bash
repowire doctor
```

Runs the same daemon/runtime/integration report as `status`, then checks the
host prerequisites used by the core (`tmux` and `git`). It exits non-zero when
the daemon is unavailable; missing optional host tools are reported as warnings.

## `repowire peer`

```bash
repowire peer new PATH [--backend BACKEND] [--profile PROFILE] [--circle CIRCLE] [-m MESSAGE]
repowire peer list [--show-offline] [--include-hidden] [--source S]  # god-view list (all circles, includes caller)
repowire peer describe NAME_OR_ID [--circle C]  # daemon state for one peer
repowire peer unregister NAME_OR_ID         # explicitly close and retire this peer identity
repowire peer ask NAME QUERY [--timeout SEC] [--circle C]  # blocking compatibility ask
repowire peer claim-role orchestrator [--peer NAME_OR_ID] [--circle C] [--force]
repowire peer restart NAME_OR_ID [--circle C] [--fresh] [--dry-run] [--no-wait] [-m MESSAGE]
repowire peer restart NAME_OR_ID --status | --cancel  # latest result / cancel an in-flight restart
repowire peer doctor NAME_OR_ID [--circle C] [--json] [--fix]  # deep diagnostic + contradictions
repowire peer rehook NAME_OR_ID [--circle C] [--apply]         # re-establish inbound ws-hook (non-destructive)
repowire peer prune [--dry-run] [--force]   # confirm, then remove offline peers
repowire peer whoami [--register --backend B --name NAME --circle C --path P]  # read-only identity, or self-register
repowire peer asks [--peer-id ID | --pane-id PANE | --peer NAME] [--direction inbound|outbound|both] [--json]
repowire peer deliveries [--peer-id ID | --pane-id PANE | --peer NAME] [--json]
repowire peer ack CORR_ID [-m MESSAGE] [--from-peer NAME]
repowire peer decline CORR_ID --reason REASON [--from-peer NAME]
```

`peer whoami`, `peer asks`, `peer deliveries`, `peer ack`, and `peer decline` are shellable mesh diagnostics. They wrap daemon HTTP endpoints and automatically use the local `daemon.auth_token` when configured. Identity resolves in this order: explicit `--peer-id` → `--pane-id` → `$TMUX_PANE` → `--peer NAME`.

Commands that need a sender identity resolve `$TMUX_PANE` to its registered canonical `peer_id`; they fail with a registration hint instead of lazily creating another runtime identity. Outside tmux they use the explicit `repowire-cli` admin identity.

`peer deliveries` drains one-shot queued deliveries for a peer. Draining deletes the queued rows to avoid duplicate paste/replay. For queued asks, `peer deliveries` shows the original ask text once, while `peer asks` continues to show the open ask until the agent closes it with `peer ack` or the MCP `ack` tool.

`peer list` is god-view: it returns every peer regardless of circle and includes the calling shell. The MCP [`list_peers`](mcp-tools.md#list_peers) tool defaults to a peer-facing view (online only, caller hidden). Both hide peers nobody can send work to unless `--include-hidden` is given: peers that cannot receive direct input (Codex multi-agent sub-agent threads), runtime-internal `system` threads, and sub-agents whose parent is offline. Interactive output tags such rows, for example `[no-input sub-agent Pasteur]` or `[system]`, and redirected TSV appends `source`, `initiator`, `addressable`, and `parent_peer_id` columns. `--source hook|codex-app-server|unknown` narrows by origin.

`peer unregister` is an explicit operator close, equivalent to the dashboard's **Unregister peer** action. It retires the resolved `peer_id` before removing it, so a stale pane-less bridge or ws-hook cannot immediately recreate the closed peer. A genuinely new runtime session receives a new identity. Internal `POST /peer/unregister` remains the non-terminal service-reconnect primitive.

`peer ask` is a blocking CLI compatibility helper for quick manual checks. It uses the daemon's ask/answer lifecycle under the hood, waits for the recipient to `ack` with a reply, then prints the reply text. `--circle` disambiguates only within the caller's authorized scope; an ordinary registered peer cannot use it to cross a circle boundary. For agent-to-agent work, prefer the MCP [`ask`](mcp-tools.md#ask) tool, which returns a correlation id immediately and lets the conversation continue asynchronously.

`peer new` spawns a tmux-backed peer through the daemon `/spawn` route using the configured `daemon.spawn.commands.<backend>` command. Inside tmux it discovers the current session or window according to `daemon.circle_boundary`; window mode also forwards the current pane internally so the daemon can place the new peer in that window. There is no separate window flag. Outside tmux, pass an explicit circle in session mode; window mode requires tmux window evidence. Pass `--profile NAME` to append args from `daemon.spawn.profiles.<backend>.<name>`, such as a faster or more capable model selection. `--command` remains accepted as a deprecated explicit override and bypasses daemon registration/profile resolution. `-m MESSAGE` is an opening prompt: the daemon waits for the spawned peer to register, then notifies it from your peer identity (the response's `seed_state` is `awaiting_registration`); the outcome is journaled as a `spawn_seed_delivered` or `spawn_seed_failed` event.

`peer describe` accepts either a display name (`clitcoin-claude-code`) or a peer
id (`repow-5-abd4d21e`). Pass `--circle` when a display name is ambiguous across
circles; the daemon refuses to guess. Output is the canonical `GET
/peers/{identifier}` state, including identity, liveness, inbound health, and
runtime metadata. Describe also includes `restart`: `restartable`, a dry-run `preview` of the reconstructed launch, and the `last` restart result, including a failed one.

`peer claim-role orchestrator` repairs an existing registered peer when the durable session mapping has the wrong role after daemon restart. It updates the live peer and its persisted session mapping, demoting offline or stale disconnected orchestrator holders in the same circle. It refuses to demote a fresh or transport-connected online/busy holder, even with `--force`; stop the current holder first if you intentionally need to replace it. Omit `--peer` only from inside a registered peer shell where Repowire can discover the current peer.

`peer restart` stops a peer's runtime and relaunches it with backend resume in a new pane of the same tmux window. The daemon runs the whole sequence, so a peer (including the orchestrator) can restart itself: the caller may die mid-way and the daemon still finishes and notifies the resumed session with the result.

The launch line is reconstructed, never replayed from a recorded argv: the backend's `daemon.spawn.commands` entry (plus the spawn profile the pane was launched with, when Repowire spawned it, which the relaunched pane keeps), the working directory from the session binding, the permissions, and the resume id. Restart always makes a best guess: a missing input gets a default and is listed in `defaulted` (`command`, `permission_mode`, `resume_id`). The working directory is daemon-observed, so restart does not apply the `allowed_paths` gate.

Permissions never escalate. When the live runtime's argv is readable, restart removes every permission-family argument from the template (`--dangerously-*`, `--permission-mode`, `--allowedTools`, codex `--sandbox`/`-s`, `--ask-for-approval`/`-a`, `--full-auto`, `--yolo`, `-c` overrides naming sandbox or approval) and appends exactly the ones the runtime was observed with. A recognised set is named (`bypassPermissions`, `plan`, `bypass`, `full-auto`, ...); no permission arguments means `default` and none are added; anything else is carried over word for word as `preserved-verbatim`. Without argv it uses the `permission_mode` the Claude hook recorded, else the template unchanged (`defaulted: permission_mode`). On macOS argv is read through `ps`, which joins arguments with spaces, so permission values containing whitespace are not supported there.

The resume id comes from the binding's `resume_capability`, else its runtime session id, else the peer's `hook_session_id`. It is validated against the backend's session file after the old runtime has exited (SIGTERM gives it time to flush); a missing id or an absent file gives a fresh launch with `resume_mode=fresh` and a `resume_reason`. Any session-selecting arguments already in the template (`--resume`, `--continue`, `--session-id`, codex `resume <id>`, `--session`) are removed first, so a resumed launch carries only the new id and `--fresh` carries none.

| Backend | Resume command | Session store validated |
|---------|----------------|-------------------------|
| claude-code | `claude --resume <id>` | `~/.claude/projects/<cwd>/<id>.jsonl` |
| codex | `codex resume <id>` | `~/.codex/sessions/**/rollout-*<id>.jsonl` |
| opencode | `opencode --session <id>` | `~/.local/share/opencode/storage/session/*/ses_<id>.json` (id + directory) |
| pi | `pi --session <id>` | `~/.pi/pi-acp/session-map.json` (cwd + sessionFile) |

Stages, one attempt each: `prepare` (resolve, destructive pane proof, plan, then reserve the new pane by splitting the old pane's window), `exit` (SIGTERM to the runtime pid proven to live in the pane, or `tmux kill-pane` on the proven pane when no pid is known; never SIGKILL), `wait` (up to 20s for the pid, then the pane, to go), `launch`, and `register` (up to 90s until the runtime has registered on the new pane and its ws-hook holds the peer's socket; the orchestrator role is re-claimed if the old peer held it). Destructive authority is not guessed: a live runtime needs the same pane proof as `kill`, and prepare refuses (`runtime_alive_pane_unseen`) when the peer is not provably gone but tmux cannot see its pane. Every tmux call is bounded (5s).

The result is typed: `outcome` is `planned` (dry run), `in_progress`, `restarted` (with `old_peer_id`, `new_peer_id`, `runtime_session_id`), or `failed` with `failed.stage`, `failed.reason`, `failed.hint`, and `failed.last_known` (`pid_alive`, `old_pane_exists`, `new_pane_exists`, `peer_status`, `binding_status`). `would_run` carries the backend, cwd, command, permission mode and its source, resume id, the names (not values) of injected env vars, and `manual`, a `cd … && …` line. The command in `would_run` and in the journal has argument values redacted (env assignments, every flag value except `--resume`, `--session`, `--permission-mode`, `--model`, `--sandbox`, `--ask-for-approval`, and positional words that are not plain ids or subcommands); `redacted` lists them, so fill those from `daemon.spawn.commands` before running `manual`.

A failure never retries the launch and never rewrites the old binding. `needs_manual_relaunch` is true only when the old runtime is gone and nothing was launched (a `launch` failure, or a cancel between exit and launch); the CLI then prints the hint and the `manual` line and exits 1. A `register` failure is not a manual relaunch: the runtime was launched in `new_pane_id` and may still be starting, so inspect that pane first; a second launch would run two runtimes on one session. A `wait` failure means the old runtime was signalled but had not exited when the wait ended; it may still exit.

The restart fences the execution it replaced. Offline posts carry the posting pane and agent pid; terminal offline posts from the replaced pane or pid (the old ws-hook's `agent_exited`, the old runtime's `SessionEnd`) are ignored for that `peer_id` for the daemon's lifetime and journaled as `peer_restart_offline_ignored` with the `restart_id`. Posts from the replacement are never fenced. Hooks from a binary older than this change send no pane or pid and are not fenced.

While the peer is between runtimes the daemon marks it offline and closes the old socket, so notifies and asks sent in that gap are queued and replay when the replacement's ws-hook connects; delivery in the gap is queued, not guaranteed. The "you were restarted" notify is sent only after the new socket is up. Open asks involving the peer stay open under its `peer_id`; the result's warnings say so.

`--no-wait` returns `in_progress` at once; `--status` shows the latest result (also under `restart` in `peer describe`); `--cancel` cancels an in-flight restart and returns within about half a second with the state it reached. Daemon shutdown stops restart intake and cancels in-flight restarts before draining, so their typed failure reaches the journal. Restart is same-host only. The same resume-safety check is shared by restart, durable/recurring jobs, and session control.

`peer doctor` is the operator-facing counterpart to automatic lazy repair. It runs lazy repair first, re-resolves the peer, then reports registry identity, inbound reachability (WebSocket connection, tmux pane existence, pane hook metadata, agent pid liveness), pending ask state, and any detected contradictions. Contradiction codes: `ONLINE_BUT_NO_WS`, `PANE_MISSING`, `AGENT_PID_DEAD`, `HOOK_PEERID_MISMATCH`, `WS_PANE_MISMATCH` (warning), `STALE_PENDING_ASK` (warning), `NOT_RESTARTABLE` (warning: `peer restart` would fail at prepare), `RESTART_DEFAULTS` (info: restart would default some inputs), `NEEDS_MANUAL_RELAUNCH` (the last restart failed after the old runtime exited; the detail is the command to run). Local-only probes (tmux, hook metadata, pid) degrade to `unavailable` for peers on a different machine than the daemon. The command exits non-zero when any error-severity contradiction is present, so it is usable as a health gate. `--json` emits the raw report. `--fix` attempts a non-destructive `peer rehook --apply` when an inbound-down contradiction (`ONLINE_BUT_NO_WS` / `PANE_MISSING`) is found.

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

Shows the recorded delivery stages for one message — an ask (use its `correlation_id`) or a notify (use its `delivery_id`, returned in the `/notify` response). Stages are ordered (`created → resolved_peer → routed → websocket_sent → thread_input_accepted → … → acked → closed`, plus failure stages `resolve_failed`, `no_connection`, `injection_failed`). `thread_input_accepted` means a native Codex thread or Claude inbox accepted the input; `injection_failed` records a failed/rejected receipt. Historical ledgers may contain `pane_injected` from releases that supported tmux keystroke delivery. This reads the local delivery trace ledger (`GET /traces/{trace_id}`), which is not pruned. Currently covers ask and notify; query/broadcast terminal stages are not yet traced. Exits non-zero if any stage failed.

## `repowire route`

The routing config ([`routing`](configuration.md#routing)) and offline research tools. None of these commands touches the daemon.

```bash
repowire route show
repowire route replay --arm ARM [--recorded] [--mask FIELD,...] [--since RFC3339] [--limit N] [--db PATH | --input FILE] [--where hub_bound=BOOL] [--arrival-seconds N] [--allow-paid] [--json] [--rows]
repowire route reconstruct --db PATH --out FILE [--live-window DUR] [--exchange-ttl DUR] [--working-window DUR] [--json]
repowire route decisionbench --cards DIR --delegations FILE --orchestrator MODEL --arm ARM [--mask FIELD,...] [--limit N] [--allow-paid] [--json] [--rows]
```

`show` prints the effective routing config, which is what the daemon runs after any degradation: the strategy, recording, shadow arms with their sampling, the timeout, whether service targets are allowed, and each model with whether its key is set (never the key). Below that, under `IGNORED`, it lists every configured setting that is not running, followed by the migration and startup warnings. The daemon reads the config at startup, so a change takes effect after a restart.

`replay` opens `state.db` read-only, loads every recorded `route_decision`, and scores an arm (or `--arm all`) against the sender's declared target: fidelity@1 and fidelity@3, the same with fallbacks excluded, mean candidates and latency, and per-reason skip counts. Only explicit asks whose declared target was eligible count. By default the arm is re-run on the recorded candidate set, one API call per ask for model arms. `--recorded` instead scores the arm's recorded shadow results and makes no calls. An arm not sampled for an ask is skipped as `not_sampled`; one that was planned but has no row (daemon restart, or still running) is `planned_not_run:missing`; one that never ran because the shadow pool was full or its breaker was open is `planned_not_run:saturated` / `planned_not_run:breaker_open` (`not_run:*` when re-running). A breaker opened only by another model of the same provider (for example `clef-flash-*` gated by `clef` failures) is suffixed `:other_model`. A fallback always counts as a miss. Every ask is also placed in a delivery cohort from positive evidence in its trace: `delivered` (an accepted receipt, or the recipient closed it), `sent_unverified` (written to the socket, no receipt), `send_failed`, `no_connection`, `quiesced`, or `unknown` (no evidence either way, e.g. a crash after the decision or a delivery still in flight). All cohorts stay in the scored totals and are listed separately below the table and under `cohorts` in `--json`. The table has a column for every skip reason that occurs. Arms that read the ask text skip rows recorded with `record_text: false`. `--rows` emits one JSON line per decision with its outcome. A fallback row also carries `fallback_status`, `fallback_provider` and `fallback_code`, and `--json` counts fallbacks per reason by status and code under `fallback_codes`; the table shows neither. No recorded text is exported. Rows recorded before structured attribution kept the provider's raw error text: for those, only the status and allowlisted codes are recovered from it, and the text itself is never re-exported.

`--since` and `--limit` narrow the rows scored, not the history: the majority baselines vote from every labelled ask in the ledger, so `--limit 1` does not leave `majority-past` without history. The table prints each majority baseline's vote population (`history`: labelled asks, first and last timestamp), as does `--json`. When reporting results, disclose that window. `majority-past` only ever counts asks before the scored one; whole-window `majority` also counts later asks, including ones outside the scored window. In `decisionbench` the history is every delegation in the file, whatever `--limit` scores.

Seven baselines are computed from the ledger itself, cost nothing, and always re-run (also under `--recorded`, and first in `--arm all`): `random` (seeded by the trace id), `recent` (the candidate with the newest `last_exchange_with_sender_at`), `majority` (the candidate the sender addressed most often among the replayed asks, leaving the scored ask out of its own count; asks from after it vote too, so it is a retrospective ceiling for habit, not a predictor anyone could deploy), `newest` (the candidate with the smallest `last_seen_age_s`, ties in `peer_id` order: the "whoever is newest online" rule, which needs no sender history), `novelty` (uniform among candidates that no earlier ledger row involved as sender or declared target, scored by expectation, no seed), `hybrid-arrival` (`majority-past`, unless a never-asked candidate was seen under `--arrival-seconds` ago, 600 by default; then uniform among those arrivals; the threshold is pre-registered as of 2026-10-13 for live validation and must not be re-tuned on live rows), and `majority-past` (the same, but only asks decided strictly before the scored one vote: the causal, deployable history baseline). `recent` and the majority baselines rank only candidates they have evidence for and fall back with `no_history` (a miss) when there is none. They are the controls for attested arms: an attested arm that cannot beat `recent` is measuring recency. Every row carries `declared_is_recent_contact` (the sender picked the candidate it exchanged with most recently, read from the unmasked snapshot), and the table and `--json` (`recency`) split the scored asks into `declared_recent_contact`, `declared_other`, and `no_exchange_data` (no candidate had a recorded exchange, for example right after a daemon restart, since exchange history is held in memory); each row's stratum is in `recency`. `declared_other`, where the sender went against recency, is the informative one.

`--mask FIELD,...` blanks candidate fields before a re-run arm sees them, including the state sent to `jev-*`, `frontier-*` and `clef-*` templates. Maskable: `backend`, `description`, `description_age_s`, `last_exchange_with_sender_at`, `last_seen_age_s`, `open_asks_inbound`, `path`, `project`, `role`, `status`, `turn_state`. A mask is global to the request body: the sender block is blanked exactly like the candidates, so a hidden field cannot leak through it (before this was fixed the sender kept its own activity fields). A masked run names its mask in the table header, in `--json` (`mask`), and on every `--rows` line. `--mask` with `--recorded` is refused, since recorded results were produced unmasked. Example: `repowire route replay --arm jev-attested --mask last_exchange_with_sender_at,open_asks_inbound`.

`replay` and `decisionbench` never spend by default. Re-running a model arm (`jev-*`, `frontier-*`, `clef-*`) is one paid provider call per row, so it needs `--allow-paid`. Without it `--arm all` runs every free arm (the seven baselines, plus every arm under `--recorded`, which makes no calls), names the skipped arms on stderr, and a named paid arm is refused. On exit, including after Ctrl-C (which stops scoring and reports the rows done so far, exit code 130), stderr lists per-provider call counts and the sum of provider-reported `input_tokens`. A call is a request that left the process, answered or not; `reported on N` says how many calls carried a token count (Jev and Clef report one; Frontier does not yet).

### Reconstructing history (`route reconstruct`)

`delivery_traces` has no candidate snapshots before the router recorded them, but the `events` journal does hold every `ask` plus the activity around it. `reconstruct` rebuilds a `Decision` per ask from the journal and writes JSONL for `route replay --input`. It opens `--db` read-only; point it at a copy of a live `state.db` made with `sqlite3 state.db ".backup copy.db"`. `replay --input` accepts `--where KEY=VALUE[,KEY=VALUE]` over `hub_bound`, `sender_type` and `recency` to score a subset; the majority baselines still vote from every row. Every report row carries `chance_at_1`/`chance_at_3`, the analytic expectation of a random ranking over the same scored rows (mean of 1/n and min(3,n)/n); read lift against it, not against the single seeded `random` draw.

Nothing in the journal says who was registered, so every field is an inference. The assumptions, all adjustable:

- **Excluded, in this order, each counted:** sender is a machine identity (`jobs`, `daemon-job-runner`); `self_target`; no `to_peer_id`; declared target is a service (`jobs`, `telegram`, `dashboard`, `slack`, by parsed project).
- **Online at t.** `peer_online` and `peer_status` rows only begin 2026-08-11, after the first ask window ends, so liveness comes from activity. A peer's activity is what it produced: its `chat_turn`s and the asks, acks and notifications it sent. It is a candidate at t if its last activity before t is within `--live-window` (default 24h) and no `peer_offline` or `peer_reaped` for it lies between that activity and t. Later activity revives it. Peers that registered and never acted before t are invisible, and a candidate set built this way is a lower bound on the real registry. Machine identities and the sender are never candidates. Events at exactly t are not yet known.
- **Declared target not online at t** is kept apart and never scored, with its evidence: `no_prior_evidence` (no activity before the ask, usually a fresh spawn), `offline_after_last_activity`, `stale_beyond_window`. These rows are in the output with `target_online_at_t: false` and replay skips them as `target_not_online`.
- **Fields.** `display_name` is the name the peer carried at t. `backend` and `project` are parsed from the convention `<project>[-<n>]-<backend>` (backend one of `claude-code`, `codex`, `opencode`, `pi`; a trailing `-<n>` is dropped). `path`, `description`, `description_age_s` are empty and `ephemeral` is false. `last_seen` is the peer's last activity. `status` is `online`; `access` is `ok`; `role` is `service` for the service names above, else `agent`.
- **`turn_state`.** `chat_turn` rows are written when a turn is flushed, at its end, so the journal cannot show a peer mid-turn. `working` means the peer's last chat turn before t was an assistant turn within `--working-window` (default 5 minutes) and `idle` means any older last turn. It is a "just finished" proxy for recent activity, not the live hook state. Empty when the peer had no chat turn before t.
- **`last_exchange_with_sender_at` and `open_asks_inbound` follow the live AskTracker** (`service/ask_tracker.go`, `RoutingStats`) event for event: the tracker holds *asks only*, so notifications and bare acks are not exchanges. Each ask between the sender and the candidate (either direction) counts at its creation time, or at the time of the first ack that carried a message (`has_message`: `ack_with_msg` and `declined`), which is when the tracker captures `RepliedAt`. An ask expires `--exchange-ttl` (24h) after its *creation*, whatever happened since. An ask is open until any ack closes it or a later ask names it in `reply_to`; `open_asks_inbound` counts open asks to the candidate from anyone, self-addressed ones included. A self-addressed ask does not set the sender's own `last_exchange_with_sender_at` (live would, through the tracker's sender-equals-candidate case): the sender block differs from live in that one field. Not reconstructable: the live tracker is in memory and a daemon restart wipes it, and restart times are not in the journal, so the reconstruction behaves as if the daemon never restarted (richer exchange data than live, fewer `no_exchange_data` rows). Asks closed by `answered`, `send_failed` or eviction before the 24h mark are not in the journal either.
- **Strata on every row:** `sender_type` (`orchestrator`: name starts with `orchestrator`; `human_relay`: telegram/dashboard/slack/jobs bridge; `peer`: everything else), `hub_bound` (target name starts with `orchestrator`), `recency` (as above, computed over the reconstructed candidates), `target_online_at_t` and `target_evidence`.
- **Identity is `peer_id`.** Spawned job peers reuse display names with a fresh `peer_id` per run, so targets and majority votes are per `peer_id`; the distinct-name count in the summary shows the gap.

The summary accounts for every ask in exactly one of: excluded (by reason), target not online (by evidence and hub/worker side), or scorable (by hub/worker side and recency stratum), with the candidate-set size distribution and distinct sender and target counts.

**Prior-ask counts on live snapshots.** Every candidate in a `route_decision` row (and the sender block) carries `prior_asks_total` (asks ever addressed to that peer, from anyone), `prior_asks_sent` (asks it ever sent) and `prior_asks_from_sender` (the addressed ones this sender sent; omitted when the sender is unresolved). They are read from the durable stores: the events journal (all asks before 2026-08-10) and the delivery-trace ledger's `created` stage (after), merged once per correlation id. The daemon loads them in the background at start, on a separate read-only connection to the state file so the store's own connection is never held up, and then keeps them current as asks are registered. The loader runs under the daemon's context, its retries and backoff are cancellable, and shutdown waits for it at most two seconds. A decision never waits for the load: until it finishes, rows omit the fields and replay treats them as `ledger`. The ask being decided is snapshotted before it is registered, so it never counts itself, and a retry of a correlation id does not count twice. `ask_many` children, legacy `/query` asks and blocking questions are never counted: they write no `created` stage, so counting them live would make the counts disagree across a restart. When a row carries `prior_asks_total` and `prior_asks_sent`, `novelty` and `hybrid-arrival` call a candidate never asked when they sum to zero (the same as the historical analysis: never the sender or target of an ask), instead of consulting earlier replay rows, and each replay row's `never_asked_source` says `field`, `ledger` or `mixed`. Every `novelty` and `hybrid-arrival` row, scored or fallen back, carries `never_asked_source`. Rows recorded before the fields existed, rows made while the history was loading, and reconstructed rows use the ledger. Live validation of the pre-registered trigger starts at the first row that carries the fields.

**`hybrid-arrival` reference numbers (pre-registered 2026-10-13).** "Never asked" is read from the replay ledger, because live snapshots carry no prior-ask count yet. On a short ledger the trigger over-fires: early rows see every peer as new, and the hybrid then loses to `majority-past` by about 10 points on continuation and orchestrator-sender rows. The reference for live rows recorded under the current daemon is therefore the ledger-defined historical result (f@1, fallbacks as misses, threshold 600 s): worker-bound .511 (`majority-past` .560), switches .258 (.254), orchestrator-sender switches .284 (.339), switches to a never-asked target .380 (0). The design as analysed used a full-journal definition of "never asked" (all 2,545 asks, machine dispatch included) and reached .568, .326 and .392 on the same sets; live rows cannot test that figure until candidate snapshots record a prior-ask count. Do not re-tune the threshold on live rows.

`decisionbench` runs the same scoring on [DecisionBench](https://huggingface.co/decisionbench) Stage-2 delegations as a stateless sanity check. Download one card flavor (`decision_bench/profiles/c2_static/` from `decisionbench/code`) and a Stage-2 zip (from `decisionbench/data`); the dataset is not redistributed with Repowire. Each delegation becomes one decision whose label is the orchestrator's own pick, with every other carded model as a candidate described by its card. There is no attested state on that substrate, so attested-only arms are uninformative there.

## `repowire why`

```bash
repowire why [COMMIT] [--json]
```

Replays the mesh conversations behind a commit. Run `repowire setup --git-hooks` in each repository to install a native Git `prepare-commit-msg` hook. With `experiments.git_trailers` enabled (the default), it adds one `Repowire-Thread: <correlation_id>` trailer per closed ask involving the committing pane’s registered peer since that repository’s HEAD commit, plus a `Repowire-Session: <repowire_session_id>` trailer. This works across agent backends that inherit the registered tmux pane; Git supplies the actual repository context, including `git -C` and linked worktrees. No shell commands are rewritten.

The hook preserves messages already carrying Repowire attribution and skips reused messages (`--amend --no-edit`, `-c`/`-C`), merge/squash messages, and rebase/cherry-pick/revert replay. An amend with a fresh `-m` message is treated as a new message. Empty messages stay empty. Missing identity, disabled configuration, or unavailable history leaves the message unchanged; operational errors warn without blocking the commit. Trailers are prepared before the editor opens and can be edited by the author.

For an existing hook or hook manager, add `repowire hook prepare-commit-msg "$@"` to its `prepare-commit-msg` entry point. Setup never overwrites an existing hook or changes `core.hooksPath`. Remove the generated hook to uninstall, or disable `experiments.git_trailers` to leave it inert. Re-run ordinary `repowire setup` when upgrading from the Bash-rewrite version to remove its obsolete PreToolUse registration.

`why` reads those trailers with `git log --format=%(trailers:...)` (defaulting to `HEAD`), fetches each thread from the delivery trace ledger (`GET /traces/{correlation_id}`), and prints who asked what and how it closed. A thread missing from the ledger prints an error line instead. Outside a git repository, or for a ref git cannot resolve, it exits non-zero with a clear message. Plain git works too: `git log --format='%h %s%n%(trailers:only)'` lists the trailers without the daemon.

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

Run the Telegram bot peer manually. Reads `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` from the environment or `~/.repowire/config.yaml`. When both values are configured, setup installs Telegram as an OS-managed user service alongside the daemon; this command is for an intentionally separate bot process. The bot registers as the canonical `telegram` peer, and messages from it are framed as human input.

## `repowire slack start`

```bash
repowire slack start
```

Run the Slack bot peer manually over Socket Mode (no public URL needed). Reads `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, and `SLACK_CHANNEL_ID` from the environment or config. When all three values are configured, setup installs Slack as an OS-managed user service alongside the daemon.

## `repowire update`

```bash
repowire update
```

Upgrade through Homebrew when Repowire is Homebrew-managed; otherwise download
and install the latest checksum-verified native release. Then re-run
non-interactive setup. SQLite state migrations run when the daemon restarts;
the updater preserves an already-running Codex bridge, and Codex's App Server
is never restarted by Repowire, so active Codex sessions survive. Verify with `repowire doctor`.

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
