# MCP tools

Every agent in the mesh exposes the same set of MCP tools through the repowire server. Tool calls go to the local daemon over HTTP; the agent never sees daemon internals. Names and signatures are stable and used identically across Claude Code, Codex, OpenCode, and Pi.

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
A request that claims a peer (`X-Repowire-Peer`) with an identity proof that
fails validation is not downgraded to `mcp-http`: its tool calls return an
error naming the reason (`expired`, `unknown`, `peer_mismatch`,
`peer_not_registered`) and are journaled as `mcp_identity_proof_rejected`.

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
ask(query: str, peer_name: str | None = None, reply_to: str | None = None, circle: str | None = None, attachments: list[dict] | None = None, fallback_to: str | None = None) -> str
```

Open a non-blocking ask thread. In normal use, you tell your local agent what you need in natural language, and the agent invokes this MCP tool. Returns a `correlation_id` immediately. The recipient closes the thread with `ack`; the daemon routes the close back as a notification framed `[ack #cid from @peer]`.

`peer_name` is optional. Leave it out to let the configured [routing strategy](configuration.md#routing) choose the recipient. The result then reads `ask-<id> (routed by <arm> to @<peer>)`, and `fallback_to` names the peer that receives the ask if the strategy cannot choose. Under the default `static` strategy, an ask without `peer_name` is refused with `no_target`. A named peer is never rerouted, and an empty `peer_name` is rejected. `peer_name: null` is rejected: only leaving the key out hands the ask to the strategy. A key that only resembles `peer_name` (for example `peerName`) is also rejected. A strategy never delivers to a service- or human-role peer unless `routing.allow_service_targets` is set; naming one always works.

Use `ask` for worker checkpoints, review requests, pre-commit handoffs, status checks you intend to track, and delegated work where closure matters. Use a durable job instead when the work needs lifecycle and result state.

Daemon events for asks and acks include nullable `repowire_session_id`, `from_repowire_session_id`, and `to_repowire_session_id` fields when an existing session binding can be resolved. Peer IDs remain the routing authority.

Live delivery is attempted first. A busy peer with native active-delivery support, including current Claude inbox and Codex App Server peers, accepts the message immediately; other busy hook-backed peers defer it until the active turn ends. If a CLI-fallback/polling peer has no live transport, the ask stays open and a one-shot queued delivery is stored in SQLite for its next Stop-hook or CLI drain. The queued delivery is deleted after drain; the ask itself still appears in `/asks/pending` until `ack`. Asking your own peer is rejected.

Peer resolution defaults to the caller's circle. Ordinary peers cannot target another circle explicitly; passing a foreign `circle=` is rejected. Callers or targets whose role bypasses circles (`orchestrator`, `service`, human surfaces) may resolve mesh-wide and pass `circle="<name>"` to disambiguate.

Pass `reply_to` to chain a follow-up: the prior thread closes and a new one opens referencing it. See [misroute refusal](../concepts/message-types.md#misroute-refusal) for what happens when names collide within the resolution scope.

```text
ask("project-b", "What API endpoints do you expose?")
# returns "ask-c1a1c7dd"
```

`peer_name` may name a peer on another machine of the same relay key as `name@machine` or `peer_id@machine`, when caller and target share a named circle ([circles across machines](../concepts/peers-and-circles.md#circles-across-machines)). The correlation id then reads `ask-<unix>-<hex>`. If the send's outcome is unknown, the result still carries the id with an `uncertain` error: do not resend; `wait_on_ack` or the asker's next turn settles it. `reply_to`, attachments and questions are refused across machines, and a cross-machine ask can only be closed by its recipient.

An ask to the human peer (or `telegram`) is stored in the [human inbox](../concepts/human-inbox.md) as an `ask` item and stays open until the user replies from any surface. The result adds the inbox receipt: `ask-c1a1c7dd inbox_item=inb-5e6f7a8b surfaces=telegram`.

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

### `decline`

```text
decline(correlation_id: str, reason: str) -> str
```

Return an ask that cannot be handled to its original asker. The ask closes with
`close_reason="declined"`, the reason is delivered back as a
`[declined #cid from @peer]` message, and the recipient's Stop-hook reminder
clears. Unlike a bare `ack`, this tells the asker that the work remains undone.

```text
decline("ask-c1a1c7dd", "Repowire rejected both acknowledgement attempts")
```

### `delegate`

```text
delegate(correlation_id: str, peer_name: str, note: str | None = None, circle: str | None = None) -> str
```

Hand an ask you received to a better-placed peer. The ask keeps its
correlation id and its asker; only the recipient changes, and the hop is
recorded. The new recipient receives the ask from you, with a first line
`[delegated by @you for @asker: note]`, the original attachments and question
(never as the asker, so your note cannot carry the asker's authority).
Your Stop-hook reminder clears and theirs starts. Their reply reaches the
original asker exactly once, framed `[ack #cid from @them via @you]`.

Delegation is refused, and the ask stays with you, when the target is unknown,
not addressable, outside your circle rules, the asker (decline instead), a peer
that held this ask before (`former_holder`: an ask never returns, so a former
holder's late reply cannot pass as current), or offline (no live connection). A busy target receives the ask through its next
Stop reminder, as a fresh ask would. Cross-machine asks, the dispatch ask of a
live job, and chains of 8 hops are not delegable. Once you delegate, your own
`ack`, `decline`, or `answer` on that ask is refused with `ask_delegated`.

```text
delegate("ask-c1a1c7dd", "parser-owner", note="owns the tokenizer")
# delegated #ask-c1a1c7dd to @parser-owner (1 hop(s)); the reply goes to the original asker
```

### `answer`

```text
answer(correlation_id: str, option_id: str | None = None, text: str | None = None) -> str
```

Answer a structured question carried on an ask. Pass `option_id` to select a choice, or `text` for a free-text answer. A bare `answer(cid)` records an acknowledged answer. Tool-permission questions also accept a denied outcome through the dashboard and Telegram renderers; ACP permission prompts deny by default on timeout.

When the asker is the human, the answer (like any `ack` reply) is stored in the human inbox as an `answer` item and threads under the human's original message on Telegram.

This is the typed counterpart to `ack` for questions such as tool approvals and future AskUserQuestion-style prompts. Plain asks still use `ack`; `/answer` rejects a plain ask so the existing `ack` retry semantics are not bypassed. If the asking peer is offline after a structured answer is recorded, the readable reply is stashed and redelivered on reconnect.

```text
answer("acpperm-8b9c1f42", option_id="allow")
answer("ask-c1a1c7dd", text="Use the staging database")
```

### `notify_peer`

```text
notify_peer(peer_name: str, message: str, circle: str | None = None, attachments: list[dict] | None = None) -> str
```

Fire-and-forget. No lifecycle, no expected response. Returns the `notif-delivery-XXXXXXXX` delivery ID, which is also its trace ID (`repowire trace <id>`), not a thread you can close. Use for FYIs, self-wakes, reminders, human phone updates, and nudges where no closure is expected.

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

To reach the user, notify the human peer: its name (`human.name`, default `human`) or the legacy address `telegram`. The message is stored in the [human inbox](../concepts/human-inbox.md) and pushed to the user's live surfaces (Telegram today). The result adds the stored item and the surfaces a push was started on:

```text
notify_peer("telegram", "deploy finished, green across CI")
# notif-delivery-1a2b3c4d inbox_item=inb-5e6f7a8b surfaces=telegram
```

`surfaces=none` means the item is stored but no surface is live; the user sees it on their next `repowire inbox` or surface connect. Per-surface outcomes are trace stages (`surface_pushed`, `surface_failed` with an error class). A notify to the human never opens an ask; use `ask` when you need the user's reply. The `dashboard` already sees agent turns; you do not need to notify it.

`ask`, reply `ack`, and `notify_peer` accept optional attachment metadata
objects (`id`, `path`, `filename`, `size`, `content_type`). Text-only calls are
unchanged; surfaces should still include a local path in text when targeting an
older transport that may ignore the structured field.

### `broadcast`

```text
broadcast(message: str) -> str
```

Fan out to every online peer in your circle. No correlation, no reply. Use sparingly — treat it as a soft interrupt for everyone in scope.

The human peer and its surfaces are left out unless the user opted in with `human.broadcast: true`. Then the broadcast is stored in the inbox, and it is pushed only to surfaces whose own `broadcast` flag is on.

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

## Messages

Every notify, ask, and ask reply delivered to an agent peer has a durable
`message_id` (`msg-<hex8>`), carried on its `<peer-message message-id="...">`
tag. Messages are kept for `retention.content_days` (7 by default). The human's messages are its
[inbox](../concepts/human-inbox.md) instead.

### `list_messages`

```text
list_messages(unread_only: bool = False, limit: int = 20) -> str
```

List the messages you received, newest first, as JSON: each with its
`message_id`, `kind` (`notify`, `ask`, `reply`), sender, `correlation_id`, a
one-line `preview`, its size in `chars`, and `read_at`. Bodies are not included.
The result also reports your `delivery` mode.

### `read_message`

```text
read_message(message_id: str, peek: bool = False) -> str
```

Return one of your messages with its body, framed exactly as its push would
have been (`<peer-message ...>`, or the plain human form). It is marked read
unless `peek`. Another peer's message id reads as not found.

### `set_message_delivery`

```text
set_message_delivery(mode: "push" | "pull") -> str
```

Choose how messages reach you. `push` (the default) puts each body in your
session. `pull` sends a one-line nudge instead, and you fetch the body with
`read_message`:

```text
[message msg-1a2b3c4d: notify from @reviewer, 1840 chars] First line of the body…
↳ read_message("msg-1a2b3c4d")
```

An ask nudge keeps its correlation id and ack hint. The mode is kept per
`peer_id` across daemon restarts.

### `set_wire_visibility`

```text
set_wire_visibility(mode: "visible" | "private") -> str
```

Choose whether orchestrators may act on your threads. `visible` is the
default. `private` protects every thread you take part in, earlier ones
included: an orchestrator still sees its envelope in
[`wire_query`](#wire_query) (who, state, age), never its text, and cannot act
on it. Kept per `peer_id` across daemon restarts; a session that comes
back under a new `peer_id` sets it again. Only you, or the operator
(`repowire wire visibility`), can change it.

## Inspection

### `list_peers`

```text
list_peers(show_offline: bool = False, include_self: bool = False, circle: str = "", include_hidden: bool = False, source: str = "") -> str
```

Returns a TSV with columns: `peer_id`, `name`, `project`, `circle`, `role`, `status`, `path`, `machine`, `description`, `backend`, `last_seen`, `turn_state`, `model`, `source`, `initiator`, `addressable`, `parent_peer_id`, `nickname`. The five provenance columns are appended after the historical thirteen, so positional consumers keep their indices.

`source` is how the peer reached the mesh: `hook` (a runtime hook in a tmux pane), `codex-app-server` (a thread the codex bridge registered from the Codex App Server, whether ChatGPT desktop, VS Code, or codex CLI), or `unknown` (registered before provenance existed). `initiator` is who opened the session: `user`, `agent` (spawned by another agent, e.g. a Codex sub-agent), `system` (runtime machinery such as helper threads the desktop app spins up), or empty when the runtime did not say. `addressable` is the runtime's own verdict on whether the peer can receive direct input; Codex multi-agent v2 sub-agent threads report `false` (`addressable_reason=subagent_direct_input_denied` on the HTTP record). `parent_peer_id` is the registered parent of a sub-agent thread, empty when the parent is not on the mesh. `nickname` is the runtime's human-readable name for a sub-agent (for example `Pasteur`); it is the only handle such threads have beyond the generated display name.

With the relay connected and the caller in a shared circle, remote peers in that circle follow the local rows, addressed as `name@machine` in `name` and `peer_id@machine` in `peer_id`, with `source=remote`. A final `# partial: m1, m2` line names machines that did not answer within 2s or past the 16-machine cap. `circle="*"` stays local. See [circles across machines](../concepts/peers-and-circles.md#circles-across-machines).

`ask`, `ask_many`, and `notify_peer` refuse a peer with `addressable=false` with `peer_not_addressable` (the JSON carries `peer_id` and `parent_peer_id`); nothing is queued, so re-send to the parent or another listed peer. `broadcast` skips such peers.

The default view lists peers someone can send work to. Hidden are non-addressable peers (an `ask` or `notify_peer` to them cannot land), `initiator=system` threads nobody opened, and sub-agents whose parent peer is offline or not registered. Pass `include_hidden=True` to see them; this is independent of `show_offline` and circle scope. `source="hook"` narrows to one source.

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

### `set_name`

```text
set_name(name: str) -> str
```

Rename the calling peer. Natural titles are normalized into address-safe lowercase names, so `set_name("Fix circle resume")` assigns `fix-circle-resume`. The rename is rejected if that name is already live in the same circle.

### `orchestrator_status`

```text
orchestrator_status(circle: str | None = None) -> str
```

Check whether a live orchestrator is present in a circle. An online/busy orchestrator counts as live while its transport is connected or its last-seen heartbeat remains fresh. Returns a TSV row with columns: `circle`, `present`, `peer_name`, `peer_id`, `last_seen`, `stale_after_seconds`. Defaults to the caller's own circle.

"Live" means a peer with `role=orchestrator`, status `online` or `busy`, and a heartbeat within `stale_after_seconds`. Use this before dispatching long-running work that assumes an orchestrator will be available to coordinate.

This is a *presence check*, not a snapshot of mesh state.

### `wire_query`

```text
wire_query(circle: str | None = None, peer: str | None = None, kind: str | None = None,
           state: str | None = None, stuck_only: bool = False, since: str = "24h",
           limit: int = 50, cursor: str | None = None) -> str
```

Orchestrator only. Lists ask and notify threads across every circle, newest activity first: who asked whom, who holds the ask now (and who held it before, after a delegation), its state, its age, the holder's assistant turns since it was assigned, and stuck signals. Returns TSV with columns `thread`, `kind`, `from`, `holder`, `via`, `state`, `age`, `holder_turns`, `stuck`, `protected`, and a final `cursor` row when there is another page (pass it back as `cursor`). Pages follow latest activity, so a thread that gets new activity while you page can move ahead of the cursor (skipped) or show twice.

The output is metadata only: no message text, delegation notes or reasons. Display names are agent-chosen and printed with control characters removed; a thread id that is not plainly printable is shown as an `h-` alias, which `wire_show` accepts.

The caller must be a registered peer with a proven identity and `role=orchestrator`, checked on every call. Anonymous HTTP MCP and ordinary agents are refused. The operator's `GET /wire/threads` route is not an agent surface: like `/traces`, it answers whoever holds the daemon token (or a local request when the token is not required).

- **States** of an ask: `sent`, `queued`, `deferred`, `delivered` (to the current holder), `delegated`, `reply_stashed` (the asker went offline before the reply), a close reason when closed (`acked`, `declined`, `answered`, `cancelled`, `expired`, `send_failed`, ...), or `unknown` when the daemon lost the open ask without recording its close. A notify is `sent`, `queued` (waiting in the delivery queue for an offline recipient), `dequeued` (the queue handed it to the reconnected peer; not a receipt, and a drain is recorded before its response reaches the peer's hook, so a lost response still reads `dequeued`), `expired` or `dropped` (left the queue undelivered: its TTL passed, or the per-peer cap or a peer that cannot take input removed it), `delivered`, `failed`, `remote_<outcome>` across machines, or `unverified` (sent on a transport that returns no receipt, or queued before the queue recorded its outcomes); read state is not tracked yet.
- **Stuck signals**, computed when read, never on a timer, with thresholds under [`wire.stuck`](configuration.md#wire): `ignored` (the holder is online and took `ignored_turns` turns since the ask was assigned to it), `slow` (no holder turn `slow_minutes` after assignment), `holder_offline`, `asker_gone` (a reply stashed `asker_gone_minutes` ago), and `undelivered` (a notify queued `undelivered_minutes` ago).
- **`protected`** names why a thread is protected, or `-` when it is not: a party, now or as recorded when the thread was created or moved, is the `human`, a `service` surface, `private` (see [`set_wire_visibility`](#set_wire_visibility)) or unknown to the daemon (`unknown_party`); or the thread was recorded without its parties (`no_provenance`: anything from before provenance was recorded, `/query` asks and `ask_many` children). Orchestrator actions ([`reassign`](#reassign-nudge-cancel_ask)) refuse protected threads, and so will future body reads. The circles recorded for each party, not the parties' circles today, are what those checks use.

### `wire_show`

```text
wire_show(thread_id: str, cursor: int | None = None) -> str
```

Orchestrator only. One thread's envelope and its stage timeline (`seq`, `at`, `stage`, `status`, `peer`, `close_reason`), oldest first, within 8,000 characters; a longer timeline ends with a `cursor` row naming the last `seq` shown. Same authorization and the same metadata-only rule as `wire_query`. When you may act on the thread, the output ends with an `action`/`arguments` table: each action's tool name and its arguments as JSON, with the values you fill in shown in angle brackets.

### `reassign` / `nudge` / `cancel_ask`

```text
reassign(thread_id: str, to: str, reason: str) -> str
nudge(thread_id: str, note: str | None = None) -> str
cancel_ask(thread_id: str, reason: str) -> str
```

Orchestrator only, with the same caller check as `wire_query`. They act on an open ask given its thread id from `wire_query`, as the operator's `repowire wire reassign|nudge|cancel` do, and the parties hear the action from you:

- `reassign` moves the ask to `to`, never back to a former holder or to the asker. The new holder gets the ask, the former holder is told a reply from it will be refused, and the asker is told in one line.
- `nudge` reminds the holder, at most once per 10 minutes per thread.
- `cancel_ask` closes the ask: the asker gets the cancellation as its outcome (`wait_on_ack` returns `cancelled`), and the holder is told to stop.

`reason` is required and `note` optional, at most 200 characters each. Each returns `{correlation_id, action, holder, notified, dropped_queued}` as JSON.

An orchestrator, unlike the operator, is refused (`wire_refused`, with the reason) when:

- the thread is protected (see `protected` under [`wire_query`](#wire_query));
- any party the ask ever had (asker, each holder, each delegator, and an orchestrator that moved it) was recorded in a circle other than yours, or the asker, the holder or an earlier holder is in another circle now. A party with no circle matches none;
- a reassign's `to` is outside your circle now, the human or a service surface, or private.

The check runs under the ask's claim, so no hand-off can add a party between the check and the action; a party's circle, role or visibility is read at that moment. A reassign records you as a party, so an orchestrator of another circle cannot act on the thread afterwards. As a recorded party only, your own later visibility does not protect the thread; your recorded one does. Every action also refuses a closed ask, a cross-machine ask, a job's dispatch ask (use the job tools) and an ask whose reply waits for an offline asker.

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

Update a job lifecycle state through `PATCH /jobs/{job_id}`. Returns the updated status JSON. Terminal jobs cannot move back to non-terminal states; same-terminal updates may add bounded metadata. A cancel-requested job cannot be moved to `queued` (409). Runner-managed updates should include the current `attempt_id` from the job prompt/status.

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

Session-boundary spawns target the exact tmux session named by the authorized circle, including numeric names such as `0` and `1`; they do not fall back to the active session or a prefix match. Session/window creation failures include tmux's diagnostic text.

Claude self-registers via `SessionStart`; OpenCode and Pi register from their native plugin lifecycle. Codex registers from its App Server thread event before the first prompt.
Its stdio MCP shim validates the bridge's daemon-minted runtime certificate for
Codex's per-call `_meta.threadId`, so tool calls from a shared App Server MCP
process resolve to that same native-thread `peer_id`.
The optional `message` is an opening prompt for every backend. The daemon launches the process and holds the message until the new peer registers on its pane; that registration delivers it as a normal notify from the spawner, bypassing the first-turn gate that holds other traffic until the seed lands. The result reports `opening_message=awaiting_registration`. A runtime can stop at a first-run prompt before it registers: Claude Code and Codex both ask whether to trust a folder they have not seen, such as a new worktree, and nothing registers until someone answers in the pane. If the peer has not registered after 90 seconds, the spawner gets a notice from `repowire:spawn` naming the pane, and the message stays held for up to 30 minutes; after that it is dropped and the spawner is told. Outcomes are journaled as `spawn_seed_waiting`, `spawn_seed_delivered`, and `spawn_seed_failed` events.

### `kill_peer`

```text
kill_peer(peer_identifier: str, circle: str | None = None) -> str
```

Terminate a peer by name or `peer_id`. Ordinary registered peers may only target their own circle; orchestrator, service, and human roles may pass another `circle`. Anonymous HTTP MCP retains the configured admin behavior. The mesh registration is always removed once the peer resolves unambiguously. The tmux pane is killed only when the daemon can prove it belongs to that peer: current/durable Repowire spawn ownership, or live pane hook metadata whose `peer_id` matches the target peer. A spawn record that names no peer, and pane metadata, count only while the peer's agent process is still running in that pane; whenever the peer's agent pid is known and it is not running in the pane, the pane is left alone. Path match alone is not destructive proof. If the pane cannot be verified, the call succeeds with a skipped tmux-kill note so stale/manual peers can be retired from the mesh without touching tmux. If `tmux kill-pane` fails after verification, the call fails loudly and leaves the peer registered for inspection. Otherwise the `peer_id` is closed, the same as `DELETE /peers/{name}`. Its runtime cannot register it again; a Codex App Server thread is interrupted and stays off the mesh until it is resumed.

### `restart_peer`

```text
restart_peer(peer_identifier: str, circle: str | None = None, fresh: bool = False,
             dry_run: bool = False, message: str | None = None) -> str
```

Stop a peer and relaunch it with backend resume in a new pane of the same tmux window. Same authorization as `kill_peer` (own circle for ordinary peers; destructive pane proof for a live runtime). Returns the typed restart result described under [`repowire peer restart`](cli.md#repowire-peer): `outcome` `planned` / `in_progress` / `restarted` / `failed`, `would_run` (including the paste-ready `manual` line), `defaulted`, `warnings`, and `failed.stage` / `failed.reason` / `failed.last_known` on failure. A caller restarting itself gets `in_progress` immediately; once the resumed session registers, the daemon notifies it with the outcome (`[repowire] you were restarted ...`). `message` is delivered after re-registration.

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

## Relay MCP

See [Relay MCP](relay-mcp.md) for the OAuth connection flow, remote tools, token refresh and revocation, and portable/Claude plugin installation. The remote surface is a scoped companion to the local daemon MCP tools.
