# MCP tools

Every agent in the mesh exposes the same set of MCP tools through the repowire server. Tool calls go to the local daemon over HTTP; the agent never sees daemon internals. Names and signatures are stable and used identically across Claude Code, Codex, Gemini CLI, and OpenCode.

## Transport and identity

The Go daemon implements the complete tool surface once, at its localhost-only
Streamable HTTP `/mcp` endpoint. `repowire setup` enables that endpoint and
generates `daemon.auth_token` when needed.

Agent runtimes still launch `repowire mcp` over stdio. That process does not
implement a second MCP server: it is a thin identity-preserving proxy. Because
it inherits the runtime's session environment and cwd, it can stamp the
canonical `X-Repowire-Peer` header before forwarding JSON-RPC to `/mcp`. This is
necessary when multiple peers share a project path; cwd alone is not identity.
The proxy revalidates its PID-bound identity certificate for every request, and
the daemon refreshes `last_seen` only after that proof succeeds.

Setup normally configures both pieces:

```bash
repowire setup
repowire service restart
```

Or by editing `~/.repowire/config.yaml`:

```yaml
daemon:
  auth_token: "rw_local_..."
  mcp_http:
    enabled: true
```

The endpoint requires `Authorization: Bearer <daemon.auth_token>` by default,
accepts only loopback callers, and is explicitly rejected by the hosted-relay
tunnel. `--http-mcp` remains accepted for setup-script compatibility but is no
longer needed.

Local clients that cannot launch the identity shim can connect directly. They
use the daemon-owned `mcp-http` identity, so they do not gain pane/session proof.

Client registration examples:

```json
{
  "mcpServers": {
    "repowire": {
      "type": "http",
      "url": "http://127.0.0.1:8377/mcp",
      "headers": {
        "Authorization": "Bearer rw_local_..."
      }
    }
  }
}
```

For Claude Code, the equivalent CLI shape is:

```bash
claude mcp add --transport http repowire http://127.0.0.1:8377/mcp \
  --header "Authorization: Bearer rw_local_..."
```

Lifecycle/admin tools such as spawn, kill, and schedule mutation are disabled
for anonymous direct-HTTP callers unless explicitly enabled with
`daemon.mcp_http.allow_dangerous_tools`. Calls through the local stdio shim carry
a registered peer identity and use the normal peer authorization path.

## Routing

### `ask`

```text
ask(peer_name: str, query: str, reply_to: str | None = None, circle: str | None = None, attachments: list[dict] | None = None) -> str
```

Open a non-blocking ask thread. In normal use, you tell your local agent what you need in natural language, and the agent invokes this MCP tool. Returns a `correlation_id` immediately. The recipient closes the thread with `ack`; the daemon routes the close back as a notification framed `[ack #cid from @peer]`.

Use `ask` for worker checkpoints, review requests, pre-commit handoffs, status checks you intend to track, and delegated work where closure matters. Use a durable job instead when the work needs lifecycle and result state.

Daemon events for asks and acks include nullable `repowire_session_id`, `from_repowire_session_id`, and `to_repowire_session_id` fields when an existing session binding can be resolved. Peer IDs remain the routing authority.

Live delivery is attempted first. A busy peer with native active-delivery support, including current Claude inbox and Codex App Server peers, accepts the message immediately; other busy hook-backed peers defer it until the active turn ends. If a CLI-fallback/polling peer has no live transport, the ask stays open and a one-shot queued delivery is stored in SQLite for its next Stop-hook or CLI drain. The queued delivery is deleted after drain; the ask itself still appears in `/asks/pending` until `ack`. Asking your own peer is rejected.

Peer resolution defaults to the caller's circle. Ordinary peers cannot target another circle explicitly; passing a foreign `circle=` is rejected. Callers or targets whose role bypasses circles (`orchestrator`, `service`, human surfaces) may resolve mesh-wide and pass `circle="<name>"` to disambiguate.

Pass `reply_to` to chain a follow-up: the prior thread closes and a new one opens referencing it. See [misroute refusal](../concepts/message-types.md#misroute-refusal) for what happens when names collide within the resolution scope.

```text
ask("project-b", "What API endpoints do you expose?")
# returns "ask-c1a1c7dd"
```

### `wait_on_ack`

```text
wait_on_ack(correlation_id: str, timeout_seconds: int = 600) -> str
```

Block inside the tool call until an ask you opened is answered or acked.
Returns JSON: `{status: "resolved" | "pending", reply, outcome, close_reason,
responder, ...}`. On overall timeout the ask stays open and `status:
"pending"` is returned — nothing is recorded, and you may call again.

Waiting switches the ask to **pull reply delivery**: the responder's reply is
retained on the ask and arrives as this tool's result instead of being
injected into your pane, so a blocked caller doesn't also get a duplicate
`[ack #cid …]` message after its turn.

This is the waiting primitive for unattended sessions, and mandatory style
for job executors: a job's fire ends when the turn ends, so a job that needs
a peer's answer must `ask` then `wait_on_ack` rather than ending its turn.

```text
cid = ask("reviewer", "Review the diff on branch fix/x")
wait_on_ack(cid, timeout_seconds=900)
# {"status": "resolved", "reply": "LGTM with one nit", ...}
```

### `ack`

```text
ack(correlation_id: str, message: str | None = None, attachments: list[dict] | None = None) -> str
```

Close an open ask. Bare `ack(cid)` signals "seen, no action needed." A reply `ack(cid, message)` closes the thread and delivers the message back to the original asker, durably queuing it when live delivery is unavailable. Replies always reach the asker regardless of circle, because the thread was established at ask-time.

When the ask carries a structured question, `ack` delegates to the typed answer path: bare `ack(cid)` records an acknowledged answer, while `ack(cid, message)` records a text answer. Use `answer` directly when selecting an option.

```text
ack("ask-c1a1c7dd")
ack("ask-c1a1c7dd", "we expose /health, /peers, /ask, /ack")
```

### `answer`

```text
answer(correlation_id: str, option_id: str | None = None, text: str | None = None) -> str
```

Answer a structured question carried on an ask. Pass `option_id` to select a choice, or `text` for a free-text answer. A bare `answer(cid)` records an acknowledged answer. Tool-permission questions also accept a denied outcome through the dashboard and Telegram renderers; ACP permission prompts deny by default on timeout.

This is the typed counterpart to `ack` for questions such as tool approvals and future AskUserQuestion-style prompts. Plain asks still use `ack`; `/answer` rejects a plain ask so the existing `ack` retry semantics are not bypassed. If the asking peer is offline after a structured answer is recorded, the readable reply is stashed and redelivered on reconnect.

```text
answer("acpperm-8b9c1f42", option_id="allow")
answer("ask-c1a1c7dd", text="Use the staging database")
```

### `notify_peer`

```text
notify_peer(peer_name: str, message: str, circle: str | None = None, attachments: list[dict] | None = None) -> str
```

Fire-and-forget. No lifecycle, no expected response. Returns a synthetic `notif-XXXXXXXX` ID for client-side tracking, not a thread you can close. Use for FYIs, self-wakes, reminders, human phone updates, and nudges where no closure is expected.

Do not use `notify_peer` for worker checkpoints, review requests, pre-commit handoffs, or delegated work that needs explicit ack; use `ask` or a durable job instead.

On the HTTP `/notify` response, `hook_delivery` may be present when the
recipient is a new enough WebSocket hook. It is a best-effort terminal injection
receipt with statuses such as `accepted`, `injected`, `rejected`, or `failed`; `null` means
the hook is older, a non-hook transport handled the notify, or no receipt
arrived before the daemon returned. When a session binding is known, `/notify`
responses and hook receipts may include nullable `repowire_session_id`,
`from_repowire_session_id`, and `to_repowire_session_id` fields for grouping.

If a hook-backed recipient without native active-delivery support is busy, `/notify` returns `delivery_state="queued"` and `reason="recipient_busy"`. Claude inbox and Codex App Server peers accept delivery while busy. An unavailable live transport similarly queues with `reason="queued_delivery"`. The notification is stored in SQLite and delivered once through the recipient's Stop hook or `repowire peer deliveries`, subject to the configured TTL and per-peer cap.

For an ACP-brokered peer (experimental), a fire-and-forget `/notify` returns `delivery_state="delivered"` with `reason="broker_accepted"` rather than `transport_delivered`. The broker accepted the prompt task, but the ACP reply is discarded for notify, so this is *not* a runtime receipt — the daemon never learns whether the runtime completed it. Clients that need a real receipt must not treat `broker_accepted` as one.

Peer resolution mirrors `ask`: ordinary peers remain in their own circle even when a foreign `circle=` is supplied, while circle-bypassing roles may target another circle explicitly.

The special peer `telegram` routes to the user's phone. The `dashboard` already sees agent turns; you do not need to notify it. Both are human-role peers and resolve mesh-wide regardless of your circle.

```text
notify_peer("telegram", "deploy finished, green across CI")
```

`ask`, reply `ack`, and `notify_peer` accept optional attachment metadata
objects (`id`, `path`, `filename`, `size`, `content_type`). Text-only calls are
unchanged; surfaces should still include a local path in text when targeting an
older transport that may ignore the structured field.

### `broadcast`

```text
broadcast(message: str) -> str
```

Fan out to every online peer in your circle. No correlation, no reply. Use sparingly — treat it as a soft interrupt for everyone in scope.

```text
broadcast("rebasing main, hold pushes for ~5 min")
```

ACP-brokered peers (experimental) are included in the fan-out: each receives the broadcast text as a fire-and-forget prompt through the broker (reply discarded), the same broker-accepted semantics as an ACP `notify_peer`. They appear in the response's `sent_to` on broker handoff, not on runtime completion.

### `ask_many` / `ask_many_result`

```text
ask_many(peer_names: list[str], query: str, circle: str | None = None, timeout_seconds: int = 300) -> str
ask_many_result(parent_id: str) -> str
```

Ask the same question to several peers in parallel under one parent (`askm-...`). Each recipient gets a normal child ask it closes with `ack`/`ack(msg)` — `ask_many` is a fan-out, not a vote: no quorum, no retry, no aggregation logic beyond collecting replies. Best-effort per peer (a recipient that fails to resolve or is the caller is recorded as a `failed` child and does not abort the rest; the peer list is deduped and bounded).

`ask_many` returns the `parent_id`; poll `ask_many_result(parent_id)` for the current rollup — per-peer status (`pending` / `acked` / `replied` / `failed`), captured reply bodies, and a `state` of `complete` / `partial` / `pending`. Timeout is lazy: a parent past its `timeout_seconds` deadline with open children reports `partial` / `timed_out` at read time (no background timer). State is in-memory and does not survive a daemon restart.

```text
parent = ask_many(["reviewer-a", "reviewer-b"], "ready to merge #42?")
# ... later ...
ask_many_result(parent)  # shows who replied, who's still pending
```

## Inspection

### `list_peers`

```text
list_peers(show_offline: bool = False, include_self: bool = False) -> str
```

Returns a TSV with columns: `peer_id`, `name`, `project`, `circle`, `role`, `status`, `path`, `machine`, `description`, `backend`, `last_seen`, `turn_state`, `model`.

`turn_state` is empty when unknown; otherwise `idle`, `working`, `awaiting_input` (peer is mid-turn waiting on user input), or `pending_first_turn` (spawn-seeded peer whose first prompt never landed — re-send via `notify_peer`).

`model` is the last observed runtime model when the backend reports one. It is
empty when unknown; Repowire does not infer it from spawn command strings.

By default returns online + busy peers in the **caller's circle** and hides the caller. Peers whose role bypasses circles (`orchestrator`, `service`, and human surfaces like `telegram` / `dashboard` / `slack`) are always visible regardless of the filter. Callers with `role=orchestrator` default to mesh-wide (`circle="*"`).

Pass `circle="*"` to widen to the whole mesh, `circle="<name>"` to scope to a different circle, `show_offline=True` to include offline peers, or `include_self=True` to include the caller's own row.

### `whoami`

```text
whoami() -> str
```

Returns the caller's own TSV row. Useful when an agent needs to know which display name it is registered under (display names get suffixed on collision: `repowire`, `repowire-2`).

### `set_description`

```text
set_description(description: str) -> str
```

Update the free-form description visible in `list_peers`. Call this at the start of a task so peers can see what you are working on without asking.

```text
set_description("rebuilding docs slice B")
```

### `orchestrator_status`

```text
orchestrator_status(circle: str | None = None) -> str
```

Check whether a live orchestrator is present in a circle. Returns a TSV row with columns: `circle`, `present`, `peer_name`, `peer_id`, `last_seen`, `stale_after_seconds`. Defaults to the caller's own circle.

"Live" means a peer with `role=orchestrator`, status `online` or `busy`, and a heartbeat within `stale_after_seconds`. Use this before dispatching long-running work that assumes an orchestrator will be available to coordinate.

This is a *presence check*, not a snapshot of mesh state.

## Lifecycle

### `job_create`

```text
job_create(title: str = "", kind: str = "general", assigned_peer_id: str | None = None, owner_peer_id: str | None = None, repowire_session_id: str | None = None, correlation_id: str | None = None, circle: str | None = None, source_kind: str | None = None, source_id: str | None = None, scope: str | None = None, visibility: str = "circle", request: dict | None = None, deadline_at: str | None = None, expires_at: str | None = None, prompt: str | None = None, prompt_file: str | None = None, path: str | None = None, backend: str | None = None, profile: str | None = None, due_at: str | None = None, cron: str | None = None, result_surface: str | None = None, process_scope: str | None = None, continuity: str | None = None, provenance: dict | None = None) -> str
```

Create a durable tracked work job through the daemon `/jobs` API. Pass `cron` to create a recurring durable job template (`cal-*`) instead of a one-shot work item; pass `due_at` for a delayed one-shot. Returns the daemon response as a JSON string with either `job_id`/`work_id`/`status` for one-shot work or `calendar_id`/`recurring_id`/`calendar` for recurring work. The MCP caller's peer ID is sent as `created_by_peer_id` when available. `path` + `backend` select a daemon-spawned executor, `prompt` or `prompt_file` supplies the execution prompt, `process_scope="per_fire"` requests a short-lived executor for each fire, and `continuity="resume"` uses backend-native runtime resume between recurring fires while `continuity="fresh"` starts without resume context. Per-fire cleanup releases only the daemon-spawned or backend-resumed executor for that fire; reused persistent executors and explicitly assigned peers stay live.

### `job_list`

```text
job_list(state: str | None = None, owner_peer_id: str | None = None, created_by_peer_id: str | None = None, repowire_session_id: str | None = None, circle: str | None = None) -> str
```

List durable jobs through `/jobs`. Returns a JSON string shaped like `{"work": [status, ...]}`. Filters mirror the HTTP API.

### `job_status` / `job_show`

```text
job_status(job_id: str) -> str
job_show(job_id: str) -> str
```

Return one job's current status JSON. `job_show` is an alias for `job_status`.

### `job_update`

```text
job_update(job_id: str, state: str, state_reason: str | None = None, phase: str | None = None, progress: dict | None = None, progress_note: str | None = None, result_summary: str | None = None, result_data: dict | None = None, error: dict | None = None, artifacts: list | None = None, provenance: dict | None = None, attempt_id: str | None = None) -> str
```

Update a job lifecycle state through `PATCH /jobs/{job_id}`. Returns the updated status JSON. Terminal jobs cannot move back to non-terminal states; same-terminal updates may add bounded metadata. Runner-managed updates should include the current `attempt_id` from the job prompt/status.

`job_update` is **optional enrichment**: fire completion is structural (the daemon arms the fire from the dispatch prompt and records the executor's final turn message as the result — see [fire lifecycle](../concepts/jobs-and-schedules.md#fire-lifecycle-structural-completion)). Use it for progress notes and structured `result_data`. The escape hatch: a fire blocked on something outside the mesh can hold itself open across turn ends with `state="running"` plus an explicit `phase`, and must then terminal-report itself.

### `job_result`

```text
job_result(job_id: str) -> str
```

Return terminal result JSON for a job, or `result_state="not_ready"` with the current status while the job is non-terminal.

### `job_cancel`

```text
job_cancel(job_id: str, reason: str = "cancel_requested") -> str
```

Request cancellation for a tracked work job. Returns status JSON. Queued jobs move directly to `cancelled`; running, delivered, awaiting-input, or blocked jobs record `cancel_requested` and remain pending until an executor reports a terminal state. When the daemon already owns a live ACP session for the job's assigned peer, it attempts a bounded protocol `session/cancel` and reports the result in `status.protocol_cancel`. If there is no live session/execution link, `protocol_cancel` reports `unavailable` rather than claiming runtime cancellation.

### `spawn_peer`

```text
spawn_peer(path: str, backend: str, profile: str | None = None, circle: str | None = None, message: str | None = None) -> str
```

Spawn a new agent session in a project directory. `backend` must have a launch profile in `daemon.spawn.commands` in `~/.repowire/config.yaml`; spawn is off by default until you configure at least one backend and one allowed path. Pass `profile` to append args from `daemon.spawn.profiles.<backend>.<profile>` for model/profile selection. If `circle` is omitted, the MCP tool uses the registered caller's current circle; anonymous HTTP MCP callers must provide one. In window-boundary mode it also inherits the caller's tmux window. Agents cannot override that scope, while orchestrators may target another circle. With the default session boundary, pass `circle="default"` explicitly to target the `default` tmux session. `command` remains accepted as a deprecated compatibility selector for one release and bypasses profile resolution.

Hook-backed runtimes self-register via `SessionStart` within a few seconds.
Codex registers from its App Server thread event before the first prompt.
Its stdio MCP shim validates the bridge's daemon-minted runtime certificate for
Codex's per-call `_meta.threadId`, so tool calls from a shared App Server MCP
process resolve to that same native-thread `peer_id`.
Antigravity is the exception while `agy` hook firing is pending upstream:
daemon spawn pre-registers it as a CLI-polling peer and returns
`registration_state=cli_fallback` plus a warning. The optional `message` is an
opening prompt for every backend; it is no longer a Codex registration seed.

### `kill_peer`

```text
kill_peer(peer_identifier: str, circle: str | None = None) -> str
```

Terminate a peer by name or `peer_id`. Ordinary registered peers may only target their own circle; orchestrator, service, and human roles may pass another `circle`. Anonymous HTTP MCP retains the configured admin behavior. The mesh registration is always removed once the peer resolves unambiguously. The tmux pane is killed only when the daemon can prove it belongs to that peer: current/durable Repowire spawn ownership, or live pane hook metadata whose `peer_id` matches the target peer. Path match alone is not destructive proof. If the pane cannot be verified, the call succeeds with a skipped tmux-kill note so stale/manual peers can be retired from the mesh without touching tmux. If `tmux kill-pane` fails after verification, the call fails loudly and leaves the peer registered for inspection.

## Review queue

### `mark_reviewed`

```text
mark_reviewed(pr_url: str, last_reviewed_sha: str | None = None) -> str
```

Record that you've reviewed a GitHub PR. After this call, the PR stops surfacing in your `review_queue` at the recorded SHA. If `last_reviewed_sha` is omitted, the daemon best-effort fetches the current HEAD via `gh api`; future pushes to the PR will then surface as `re-review-suggested`.

### `review_queue`

```text
review_queue(peer_name: str | None = None) -> str
```

List PRs awaiting your review (or another peer's). Defaults to the calling peer. Returns TSV with columns: `pr_url`, `last_reviewed_sha`, `current_head_sha`, `state`, `my_action`.

`my_action` values:

- `none-needed` — PR open, head SHA matches what you reviewed.
- `re-review-suggested` — PR open, new commits since your review.
- `merged-since-review` — PR merged after you last reviewed it.
- `closed-since-review` — PR closed (not merged) since your review.
- `unknown` — GitHub API unreachable; falls back to cached state.

## Scheduling

### `schedule_create`

```text
schedule_create(to_peer: str, text: str, fire_at: str, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a **one-shot** future message to a peer. Use `schedule_cron` for recurring schedules, or `schedule_self` when the recipient is the calling peer.

At `fire_at`, the daemon delivers `text` to `to_peer` on your behalf. `kind="notify"` is fire-and-forget; `kind="ask"` opens an ask thread (the recipient must `ack`). Use `kind="ask"` for scheduled checkpoints, reviews, and handoffs that must be closed. `fire_at` is ISO-8601; naive datetimes are interpreted as UTC.

Use for self-wake reminders, post-stand-up nudges, or future check-ins that don't need a live caller waiting. Returns a `sched-XXXXXXXX` ID; pass it to `schedule_delete` to cancel.

### `schedule_self`

```text
schedule_self(text: str, fire_at: str | None = None, cron: str | None = None, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a future message to yourself. Provide exactly one of `fire_at` or `cron`.

For one-shot reminders, pass an ISO-8601 `fire_at`. For recurring reminders, pass a five-field cron expression or an alias such as `@hourly`, `@daily`, `@midnight`, `@weekly`, or `@monthly`. `kind="notify"` delivers a reminder; `kind="ask"` opens an ask thread that must be acked. Use `kind="ask"` when the scheduled delivery gates progress.

### `schedule_cron`

```text
schedule_cron(to_peer: str, text: str, cron: str, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a recurring message to a peer. `cron` accepts standard five-field cron syntax, including ranges, steps, and comma-separated values, plus aliases such as `@hourly`, `@daily`, `@midnight`, `@weekly`, and `@monthly`.

Recurring schedules advance to their next matching fire time after delivery. Cancel them with `schedule_delete`.

### `schedule_list`

```text
schedule_list(mine_only: bool = True, include_cron: bool = False) -> str
```

List pending scheduled check-ins. Returns TSV with columns: `schedule_id`, `from_peer`, `to_peer`, `kind`, `fire_at`, `text`. Sorted by `fire_at` ascending. Pass `mine_only=False` to see all schedules on the daemon. Pass `include_cron=True` to append a trailing `cron` column for recurring schedules.

### `schedule_delete`

```text
schedule_delete(schedule_id: str) -> str
```

Cancel a pending scheduled check-in by the ID `schedule_create` returned.

## Session sharing

### `share_session`

```text
share_session(
    peer_name: str | None = None,
    permissions: str = "ro",
    ttl_secs: int | None = None,
) -> str
```

Generate a shareable relay link for a peer. **Only call when the user explicitly
asks** — do not share proactively. Requires relay to be configured.

| Arg | Description |
|---|---|
| `peer_name` | Display name of the peer to share. Defaults to the calling peer (yourself). |
| `permissions` | `"ro"` (read-only, default) or `"rw"` (read-write — viewer can inject asks). |
| `ttl_secs` | Link lifetime in seconds (must be > 0). `None` means no expiry. |

Returns the share URL and `share_id`, e.g.:

```
share link for my-agent [ro]: https://repowire.io/s/sh_xxx
share_id: sh_xxx
expires: never
```

### `revoke_share`

```text
revoke_share(share_id: str) -> str
```

Revoke a share link. Active SSE connections on that link receive a
`share_expired` event and close within the next keepalive cycle.

## See also

- The [HTTP API](http-api.md) exposes the same routing primitives for non-MCP callers.
- [Message types](../concepts/message-types.md) covers the semantics of `ask`, `ack`, `notify_peer`, and `broadcast` at a higher level.
- The [orchestrator pattern](../concepts/orchestrator.md) shows where `orchestrator_status`, `review_queue`, and the scheduling tools fit together.
- [Session sharing](../use/features/session-sharing.md) — full usage guide with examples.
