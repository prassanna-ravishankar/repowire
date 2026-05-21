# MCP tools

Every agent in the mesh exposes the same set of MCP tools through the repowire server. Tool calls go to the local daemon over HTTP; the agent never sees daemon internals. Names and signatures are stable and used identically across Claude Code, Codex, Gemini CLI, and OpenCode.

## Transports

The default, stable transport is the per-agent stdio MCP server installed by `repowire setup`.

An experimental Streamable HTTP MCP endpoint can also be mounted on the local daemon at `/mcp` with:

```bash
repowire setup --http-mcp
repowire service restart
```

Or by editing `~/.repowire/config.yaml`:

```yaml
daemon:
  auth_token: "rw_local_..."
  mcp_http:
    enabled: true
```

`repowire setup --http-mcp` generates `daemon.auth_token` if one is not already set. This endpoint is opt-in, localhost-only, requires `Authorization: Bearer <daemon.auth_token>` by default, and is not exposed through the hosted relay. HTTP MCP uses a daemon-owned `mcp-http` identity instead of tmux/cwd session inference, so it is useful for local MCP clients that cannot run the stdio server in an agent session.

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

Lifecycle/admin tools such as spawn, kill, and schedule mutation are disabled for HTTP MCP unless explicitly enabled with `daemon.mcp_http.allow_dangerous_tools`. The stdio server installed by `repowire setup` remains the stable default and still runs as `repowire mcp`.

## Routing

### `ask`

```python
ask(peer_name: str, query: str, reply_to: str | None = None, circle: str | None = None, attachments: list[dict] | None = None) -> str
```

Open a non-blocking ask thread. In normal use, you tell your local agent what you need in natural language, and the agent invokes this MCP tool. Returns a `correlation_id` immediately. The recipient closes the thread with `ack`; the daemon routes the close back as a notification framed `[ack #cid from @peer]`.

Daemon events for asks and acks include nullable `repowire_session_id`, `from_repowire_session_id`, and `to_repowire_session_id` fields when an existing session binding can be resolved. Peer IDs remain the routing authority.

Peer resolution defaults to the caller's circle. Peers whose role bypasses circles (`orchestrator`, `service`, human surfaces) resolve mesh-wide; everything else is scoped to the caller's circle so the daemon's ambiguous-resolve refusal applies. Pass `circle="<name>"` to target a different circle explicitly.

Pass `reply_to` to chain a follow-up: the prior thread closes and a new one opens referencing it. See [misroute refusal](../concepts/message-types.md#misroute-refusal) for what happens when names collide within the resolution scope.

```python
ask("project-b", "What API endpoints do you expose?")
# returns "ask-c1a1c7dd"
```

### `ack`

```python
ack(correlation_id: str, message: str | None = None, attachments: list[dict] | None = None) -> str
```

Close an open ask. Bare `ack(cid)` signals "seen, no action needed." A reply `ack(cid, message)` closes the thread and delivers the message back to the original asker. Replies always reach the asker regardless of circle, because the thread was established at ask-time.

```python
ack("ask-c1a1c7dd")
ack("ask-c1a1c7dd", "we expose /health, /peers, /ask, /ack")
```

### `notify_peer`

```python
notify_peer(peer_name: str, message: str, circle: str | None = None, attachments: list[dict] | None = None) -> str
```

Fire-and-forget. No lifecycle, no expected response. Returns a synthetic `notif-XXXXXXXX` ID for client-side tracking, not a thread you can close. Use for status pings and announcements.

On the HTTP `/notify` response, `hook_delivery` may be present when the
recipient is a new enough WebSocket hook. It is a best-effort terminal injection
receipt with statuses such as `injected`, `rejected`, or `failed`; `null` means
the hook is older, a non-hook transport handled the notify, or no receipt
arrived before the daemon returned. When a session binding is known, `/notify`
responses and hook receipts may include nullable `repowire_session_id`,
`from_repowire_session_id`, and `to_repowire_session_id` fields for grouping.

Peer resolution mirrors `ask`: defaults to the caller's circle, except for peers whose role bypasses circles (`orchestrator`, `service`, human surfaces) which resolve mesh-wide. Pass `circle="<name>"` to target a different circle.

The special peer `telegram` routes to the user's phone. The `dashboard` already sees agent turns; you do not need to notify it. Both are human-role peers and resolve mesh-wide regardless of your circle.

```python
notify_peer("telegram", "deploy finished, green across CI")
```

`ask`, reply `ack`, and `notify_peer` accept optional attachment metadata
objects (`id`, `path`, `filename`, `size`, `content_type`). Text-only calls are
unchanged; surfaces should still include a local path in text when targeting an
older transport that may ignore the structured field.

### `broadcast`

```python
broadcast(message: str) -> str
```

Fan out to every online peer in your circle. No correlation, no reply. Use sparingly — treat it as a soft interrupt for everyone in scope.

```python
broadcast("rebasing main, hold pushes for ~5 min")
```

## Inspection

### `list_peers`

```python
list_peers(show_offline: bool = False, include_self: bool = False) -> str
```

Returns a TSV with columns: `peer_id`, `name`, `project`, `circle`, `role`, `status`, `path`, `machine`, `description`, `backend`, `last_seen`, `turn_state`.

`turn_state` is empty when unknown; otherwise `idle`, `working`, `awaiting_input` (peer is mid-turn waiting on user input), or `pending_first_turn` (spawn-seeded peer whose first prompt never landed — re-send via `notify_peer`).

By default returns online + busy peers in the **caller's circle** and hides the caller. Peers whose role bypasses circles (`orchestrator`, `service`, and human surfaces like `telegram` / `dashboard` / `slack`) are always visible regardless of the filter. Callers with `role=orchestrator` default to mesh-wide (`circle="*"`).

Pass `circle="*"` to widen to the whole mesh, `circle="<name>"` to scope to a different circle, `show_offline=True` to include offline peers, or `include_self=True` to include the caller's own row.

### `whoami`

```python
whoami() -> str
```

Returns the caller's own TSV row. Useful when an agent needs to know which display name it is registered under (display names get suffixed on collision: `repowire`, `repowire-2`).

### `set_description`

```python
set_description(description: str) -> str
```

Update the free-form description visible in `list_peers`. Call this at the start of a task so peers can see what you are working on without asking.

```python
set_description("rebuilding docs slice B")
```

### `claim_orchestrator_role`

```python
claim_orchestrator_role(force: bool = False) -> str
```

Self-repair tool for the orchestrator workspace. Use it after a daemon restart when `list_peers(include_self=True)` shows the orchestrator session as `role=agent`. It targets the caller's current peer id, persists `role=orchestrator` into the session mapping, and refuses non-orchestrator workspace/session names. Pass `force=True` only when an existing fresh orchestrator holder in the same circle should be demoted.

### `orchestrator_status`

```python
orchestrator_status(circle: str | None = None) -> str
```

Check whether a live orchestrator is present in a circle. Returns a TSV row with columns: `circle`, `present`, `peer_name`, `peer_id`, `last_seen`, `stale_after_seconds`. Defaults to the caller's own circle.

"Live" means a peer with `role=orchestrator`, status `online` or `busy`, and a heartbeat within `stale_after_seconds`. Use this before dispatching long-running work that assumes an orchestrator will be available to coordinate.

This is a *presence check*, not a snapshot of mesh state.

## Lifecycle

### `spawn_peer`

```python
spawn_peer(path: str, backend: str, circle: str = "default", message: str | None = None) -> str
```

Spawn a new agent session in a project directory. `backend` must have a launch profile in `daemon.spawn.commands` in `~/.repowire/config.yaml`; spawn is off by default until you configure at least one backend and one allowed path. `circle` maps to the tmux session name and cannot be reassigned after spawn. `command` remains accepted as a deprecated compatibility selector for one release.

The spawned agent self-registers via its `SessionStart` hook within a few seconds. The `message` seeds first-turn context. Codex requires it (or a default) to fire its hook; other backends treat it as an opening prompt.

### `kill_peer`

```python
kill_peer(peer_identifier: str, circle: str | None = None) -> str
```

Terminate a peer by name or `peer_id`. The peer is always deregistered from the mesh. The tmux pane is killed only if the daemon spawned the peer via `spawn_peer` in the current daemon lifetime; externally attached peers or peers whose ownership was lost across a daemon restart are deregistered without touching tmux. Verify with `tmux list-panes` and follow up with `tmux kill-pane` if the pane survives.

## Review queue

### `mark_reviewed`

```python
mark_reviewed(pr_url: str, last_reviewed_sha: str | None = None) -> str
```

Record that you've reviewed a GitHub PR. After this call, the PR stops surfacing in your `review_queue` at the recorded SHA. If `last_reviewed_sha` is omitted, the daemon best-effort fetches the current HEAD via `gh api`; future pushes to the PR will then surface as `re-review-suggested`.

### `review_queue`

```python
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

```python
schedule_create(to_peer: str, text: str, fire_at: str, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a **one-shot** future message to a peer. Use `schedule_cron` for recurring schedules, or `schedule_self` when the recipient is the calling peer.

At `fire_at`, the daemon delivers `text` to `to_peer` on your behalf. `kind="notify"` is fire-and-forget; `kind="ask"` opens an ask thread (the recipient must `ack`). `fire_at` is ISO-8601; naive datetimes are interpreted as UTC.

Use for self-wake reminders, post-stand-up nudges, or future check-ins that don't need a live caller waiting. Returns a `sched-XXXXXXXX` ID; pass it to `schedule_delete` to cancel.

### `schedule_self`

```python
schedule_self(text: str, fire_at: str | None = None, cron: str | None = None, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a future message to yourself. Provide exactly one of `fire_at` or `cron`.

For one-shot reminders, pass an ISO-8601 `fire_at`. For recurring reminders, pass a five-field cron expression or an alias such as `@hourly`, `@daily`, `@midnight`, `@weekly`, or `@monthly`. `kind="notify"` delivers a reminder; `kind="ask"` opens an ask thread that must be acked.

### `schedule_cron`

```python
schedule_cron(to_peer: str, text: str, cron: str, kind: str = "notify", circle: str | None = None) -> str
```

Schedule a recurring message to a peer. `cron` accepts standard five-field cron syntax, including ranges, steps, and comma-separated values, plus aliases such as `@hourly`, `@daily`, `@midnight`, `@weekly`, and `@monthly`.

Recurring schedules advance to their next matching fire time after delivery. Cancel them with `schedule_delete`.

### `schedule_list`

```python
schedule_list(mine_only: bool = True, include_cron: bool = False) -> str
```

List pending scheduled check-ins. Returns TSV with columns: `schedule_id`, `from_peer`, `to_peer`, `kind`, `fire_at`, `text`. Sorted by `fire_at` ascending. Pass `mine_only=False` to see all schedules on the daemon. Pass `include_cron=True` to append a trailing `cron` column for recurring schedules.

### `schedule_delete`

```python
schedule_delete(schedule_id: str) -> str
```

Cancel a pending scheduled check-in by the ID `schedule_create` returned.

## See also

- The [typed Python client](python-client.md) exposes the same routing calls over the daemon's HTTP API for non-MCP callers.
- [Message types](../concepts/message-types.md) covers the semantics of `ask`, `ack`, `notify_peer`, and `broadcast` at a higher level.
- The [orchestrator pattern](../concepts/orchestrator.md) shows where `orchestrator_status`, `review_queue`, and the scheduling tools fit together.
