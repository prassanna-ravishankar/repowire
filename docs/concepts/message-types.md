# Message types

The daemon routes four message types. Pick by lifecycle, not by content.

## `ask`

Non-blocking. Returns a `correlation_id` immediately. The recipient closes the thread with `ack(corr_id)` (bare) or `ack(corr_id, message)` (reply). Chain follow-ups with `ask(reply_to=corr_id, ...)`, which closes the prior thread and opens a new one referencing it.

Use `ask` when closure matters: worker checkpoints, review requests, pre-commit
handoffs, status checks you intend to track, and delegated work that should not
quietly disappear. For durable implementation work with lifecycle/result state,
prefer a job instead of overloading a long ask thread.

If the recipient never acks, repowire injects a reminder block at the start of every subsequent prompt on the recipient side until the ask is acked. Tool-call detection is the source of truth — prose `[ack #cid]` mentions in agent output do not close anything, only a real `ack()` MCP call does.

When the daemon can resolve a durable session binding for either side, ask events include nullable `repowire_session_id`, `from_repowire_session_id`, and `to_repowire_session_id` metadata. Peer IDs remain the routing authority.

## `ack`

Closes an open ask thread.

- Bare `ack(cid)` signals "seen, no action needed."
- Reply `ack(cid, message)` delivers the message back to the original asker as a notification framed `[ack #cid from @peer] message`.

Replies always reach the original asker regardless of circle — the thread was established at ask-time and the routing is locked then.

Bare ack events and reply notifications carry the same nullable session metadata when a binding is known. Detached or offline peers keep those fields `null`.

## Structured questions and `answer`

Some asks carry a typed question envelope. A structured question can be an acknowledgement, a choice, free text, or a tool-permission prompt. The daemon still tracks it as an ask thread, but the close operation records a typed answer through `/answer` or the `answer` MCP tool:

- Choice answers select an `option_id`.
- Text answers carry free-form `text`.
- Tool-permission questions can be denied explicitly, and deny by default on timeout.

Telegram and the dashboard render structured questions as buttons when possible. ACP tool approvals use the same primitive: the ACP broker registers a blocking choice question, waits for the recorded answer, and maps it back to the runtime's permission decision. The older ACP permission-decision route remains as a compatibility shim.

A *blocking* transport — one that owns a suspendable call, such as an ACP runtime or a Claude Code `PreToolUse` hook — can register a question and park until it is answered. The daemon exposes this as `POST /questions/ask-blocking`: the caller posts a tool-permission question, the daemon holds the connection open up to a hard cap while a human surface or peer answers, and returns the typed answer. It is transport-neutral (the ACP broker and the hook share the same register-emit-wait core) and fail-closed (a tool-permission question denies on timeout). The Claude Code remote tool-approval hook is opt-in; see [PreToolUse approval](#pretooluse-tool-approval-claude-code) below.

### PreToolUse tool approval (Claude Code)

When `experiments.remote_tool_approval.enabled` is set, `repowire setup` registers a `PreToolUse` hook scoped to the configured `gated_tools` (mutating/shell tools — `Bash`, `Edit`, `Write`, `MultiEdit`, `NotebookEdit` — never read-only tools). Before a gated tool runs, the hook posts a blocking question to the daemon and waits; an allow from a human surface or peer returns `permissionDecision=allow`, and a deny, timeout, or daemon-unavailable returns `permissionDecision=deny`. Because the decision rides the hook rather than Claude's native approval, it still gates under `--dangerously-skip-permissions`.

Plain asks still close with `ack`. `/answer` rejects a plain ask so a reply cannot bypass `ack`'s delivery/retry contract. Structured answers are recorded before the human-readable reply is delivered back to the asking peer; if that peer is temporarily offline, Repowire stashes the reply and redelivers it when the peer reconnects.

## `notify_peer`

Fire-and-forget. No lifecycle, no response expected. Returns a synthetic `notif-XXXXXXXX` ID for client-side tracking, not a thread you can close.

Use `notify_peer` for FYIs, self-wakes, reminders, human phone updates, and
nudges where daemon acceptance is enough. Do not use it for worker checkpoints,
review requests, pre-commit handoffs, or delegated work that needs an explicit
ack.

The special peer name `telegram` routes to the user's phone (if the Telegram bot is running). The dashboard already sees agent turns; you do not need to notify it.

Notify events and newer hook delivery receipts also include nullable session metadata when resolvable. Missing metadata does not change delivery semantics.

## `broadcast`

Fan-out to all online peers in your circle. No correlation, no reply. Use sparingly — treat it as a soft interrupt for everyone in scope.

## Misroute refusal

`ask` and `notify_peer` resolve peer names within the caller's circle by default; peers whose role bypasses circles (`orchestrator`, `service`, human surfaces like `@telegram` / `@dashboard` / `@slack`) resolve mesh-wide. If a name matches multiple peers within the resolution scope, the daemon refuses the call with a hint to disambiguate. Pass an explicit `circle=` argument to pick one. This prevents a silent wrong-peer delivery when display names collide.
