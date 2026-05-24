# Anya routing

Status: product/design contract for the Anya personal-assistant layer. This is
not a daemon runtime implementation, a permissions model, a durable job store,
or an ACP micro-persona contract. It defines how Anya should decide which
Repowire primitive or future job surface to use for a request.

## Problem

Anya sits above the mesh. A user should be able to ask one assistant for help
without deciding whether the next action is a direct answer, a peer ask, a new
worker session, a scheduled reminder, a durable job, or a clarification prompt.

The router is the decision layer for that handoff. It consumes the user's
request, visible mesh state, persona context, memory hints, and future job
state, then returns one explicit route plan. The plan may be executed by Anya
or shown to the user for approval depending on confidence, scope, and risk.

## Boundaries

- Jobs are the spine. Once the daemon-backed tracked-work lifecycle exists, any
  durable task should be represented by a `work_id`, not by an open chat thread.
- `ask`/`ack` remains conversational. An ask can gather information, request a
  status update, or comment on a job, but acking the ask does not complete the
  job.
- Persona SOUL.md guides identity, voice, and standing preferences. It is not a
  permission policy and must not override current user instructions.
- Mesh memory and Anya memory remain deliberate-write systems. Routing may
  propose a memory write only through the memory approval path; it must never
  write memory as a side effect.
- Permissions and scopes are deferred to the Anya scopes model. Until then,
  routes that would read private data, write state, execute tools, spend money,
  or delegate broad authority must ask the user.
- The router should not add polling loops. Watchdogs and delayed follow-ups use
  Repowire schedules or future job deadlines.

## Inputs

The router takes a bounded snapshot. It should not search indefinitely before
choosing a route.

| Input | Purpose |
| --- | --- |
| `request` | User or peer message, attachments, source surface, and explicit constraints. |
| `requester` | Human/service/peer identity, circle, active session, and reply target when known. |
| `intent` | Classified intent such as answer, delegate, track, investigate, wait, retry, remind, cancel, summarize, or clarify. |
| `scope` | Project, circle, persona, privacy, and required capability boundaries. |
| `visible_peers` | Fresh `list_peers` style rows: peer ids, names, roles, projects, backends, statuses, turn states, and descriptions. |
| `jobs` | Existing visible tracked-work records when the jobs lifecycle exists. Before then, this input is empty and the route must say it used current mesh primitives only. |
| `schedules` | Relevant pending one-shot or recurring schedules. |
| `context` | Active persona metadata, curated memory summaries or references, and recent session/timeline pointers. |
| `policy` | User preferences, approval requirements, retry limits, deadlines, and escalation rules. |

## Outputs

The router returns one primary route with enough structure for a caller to
execute or request approval.

| Field | Purpose |
| --- | --- |
| `route` | One of `answer_direct`, `ask_peer`, `spawn_peer`, `use_acp_persona`, `schedule`, `use_existing_job`, `create_job`, `ask_user`, or `decline`. |
| `confidence` | `high`, `medium`, or `low`, based on clarity, capability match, state freshness, and permission certainty. |
| `reason` | Short explanation of why this route fits better than the alternatives. |
| `target` | Peer id, session id, persona name, job id, schedule id, or user surface when applicable. |
| `action` | The concrete command or future job operation to run. |
| `approval` | `none`, `confirm`, or `required`, with the reason. |
| `fallback` | Next route if delivery, capability, or confidence fails. |
| `links` | Related ask ids, work ids, schedule ids, event ids, attachments, or session pointers. |

The route object should be machine-readable first, then rendered as a compact
human explanation. It should preserve exact ids whenever they are known. Display
names are acceptable in prose but must not be the only routing key when a
`peer_id`, `work_id`, or `repowire_session_id` is available.

## Route choices

### Answer directly

Use `answer_direct` when Anya can satisfy the request from current context,
stable product knowledge, or a small bounded inspection that does not require a
different executor.

Good fits:

- explaining current mesh concepts;
- summarizing visible state already in the prompt;
- telling the user that a requested capability is not implemented yet;
- producing a short plan that the user explicitly asked for.

Do not answer directly when the request needs live project state from another
peer, private data Anya is not scoped to read, or durable follow-through.

### Ask a peer

Use `ask_peer` for a tracked conversational handoff to an existing live peer.
The output action maps to Repowire `ask(peer_id, query)` and returns a
`correlation_id`. This is best for information gathering, review requests,
status checks, and bounded collaboration where a human-readable reply closes the
loop.

Prefer `ask_peer` when:

- the target peer is online or busy in the right project/circle;
- the task is naturally a question or review, not durable execution;
- the peer already owns the context;
- a reply is required.

Fallbacks:

- If the peer is ambiguous, route to `ask_user` for disambiguation.
- If no suitable peer exists and the task is executable, route to `spawn_peer`
  or `create_job`.
- If the peer is offline but a follow-up can wait, route to `schedule`.

### Spawn a peer

Use `spawn_peer` when the work needs a full runtime session: repository files,
tools, a long-running turn, branch state, tests, or a backend profile that an
ACP micro-persona should not own.

Prefer `spawn_peer` when:

- no existing peer is available for the project/worktree;
- the work is code or repo execution, not just reasoning;
- the requested scope is allowed by configured spawn roots;
- the task has enough context to seed a first turn.

Spawning is an execution boundary, so approval should be `confirm` or
`required` unless the user has already granted an explicit delegation scope. For
Codex-backed peers, the spawn seed must include enough first-turn context to
trigger registration and avoid `pending_first_turn`.

### Use an ACP persona

Use `use_acp_persona` when the work is small, bounded, context-light,
cancellable, and does not need a full repository runtime. This is a future
job-backed route: the durable record is still a `work_id`, while the ACP persona
is the preferred executor.

Prefer `use_acp_persona` when:

- the request maps to a known persona capability;
- inputs and expected result are compact;
- tool access is narrow and revocable;
- cancellation can be represented through the tracked-work cancel contract;
- failure can fall back to a full peer or user clarification.

Until the ACP micro-persona contract exists, Anya must not claim this route is
available. It may return `create_job` with
`preferred_executor="acp_persona:<name>"` as design intent, or fall back to
`ask_user`, `ask_peer`, or `spawn_peer`.

### Schedule

Use `schedule` for time-based delivery, watchdogs, reminders, and retry-at-time
behavior. It should map to Repowire schedule tools today and to a job's
`next_scheduled_check` or deadline metadata once jobs exist.

Choose `kind="notify"` for quiet reminders and self-wakes. Choose `kind="ask"`
only when the future delivery should open a thread that someone must close.

Schedules must include:

- target peer or user surface;
- one-shot `fire_at` or recurring `cron`;
- delivery kind;
- cancellation path or schedule id once created.

### Use an existing job

Use `use_existing_job` when the request refers to an already visible durable
work item. The router should prefer exact `work_id` matches, then explicit
links from asks/schedules/session events, then cautious title/status matching.

Good fits:

- "what happened with that migration?";
- "retry the failed import job";
- "cancel the docs sweep";
- "wait until CI finishes, then summarize";
- "remind me about the open review tomorrow."

Before the jobs lifecycle ships, the router cannot actually use this route. It
should say durable jobs are unavailable and fall back to visible asks,
schedules, events, or `ask_user`.

### Create a new job

Use `create_job` for durable work that needs ownership, status, progress,
result, cancellation, visibility, or retention beyond one conversation. This is
the default future route for assistant work that must survive session
reattachment or delayed execution.

The job request should carry:

- source request and requester;
- requested result;
- scope and permissions needed;
- preferred executor: existing peer, spawned peer, or ACP persona;
- deadline, TTL, retry policy, and schedule hints;
- initial progress note and provenance links.

Until the jobs contract is implemented, this route is design-only. Anya may
explain that the task should become a job later and use an `ask_peer`,
`spawn_peer`, or `schedule` fallback only if that fallback is acceptable without
durable job semantics.

### Ask the user

Use `ask_user` when confidence is low, targets are ambiguous, required scope is
missing, or the requested action has meaningful risk.

Ask the smallest question that unblocks routing. Do not ask for information the
mesh can determine cheaply, such as a fresh peer list or visible schedule id.

Common triggers:

- multiple matching peers or jobs;
- unclear project or worktree;
- missing deadline or desired reminder cadence;
- requested write/execute/delegate scope not granted;
- destructive cancel/retry action with uncertain target;
- conflicting persona, memory, or current-turn instructions.

### Decline

Use `decline` when the request is outside policy, requires unavailable
credentials or permissions that cannot be requested safely, or would violate a
current user instruction. A decline may still include safe alternatives such as
asking for a narrower scope or creating a reminder to revisit later.

## Confidence and fallback

Confidence should be conservative:

- `high`: exact target id or single unambiguous target, clear intent, required
  scope present, and known execution surface.
- `medium`: intent is clear but state is stale, target is inferred, or a
  non-critical detail is missing.
- `low`: target, permission, timing, or expected outcome is ambiguous.

Fallback behavior:

- High-confidence, low-risk routes may execute immediately.
- Medium-confidence routes may execute if reversible and scoped; otherwise ask
  for confirmation.
- Low-confidence routes ask the user unless the only action is a safe direct
  answer.
- Delivery failure from `ask_peer` may fall back to `schedule`, `spawn_peer`, or
  `ask_user` depending on whether the task can wait, needs execution, or needs a
  human decision.
- Missing jobs support must not be hidden. Say that durable job semantics are
  unavailable and name the temporary primitive being used instead.

## Examples

### Track

Request: "Track the release checklist and tell me when it is ready."

Route: `create_job` once jobs exist. The job owns the checklist, progress,
result, and cancellation path. Today, Anya should explain that durable jobs are
not available and can schedule a self-wake or ask an orchestrator peer only as a
weaker fallback.

### Investigate

Request: "Find out why the auth tests are failing in project-a."

Route: `ask_peer` if a `project-a` peer is already live and owns the branch.
Route: `spawn_peer` if no suitable peer exists and spawn is configured. Future
route: `create_job` when the user wants durable status/result/cancel.

### Wait

Request: "Wait for CI, then summarize the result."

Route: `create_job` with a wait phase and deadline once jobs exist. Today,
route to `schedule` for a timed check or `ask_peer` to an orchestrator if a
human-readable follow-up is acceptable. Do not poll.

### Retry

Request: "Retry the failed import."

Route: `use_existing_job` if an exact failed job is visible, then perform the
job retry operation. If multiple failed imports match, `ask_user`. Before jobs
exist, ask the user or the owning peer for the exact target rather than
guessing from old chat.

### Remind

Request: "Remind me tomorrow morning to review the Anya spec."

Route: `schedule` with `kind="notify"` to the requested user surface or Anya's
orchestrator session. Include the schedule id and cancellation path.

### Cancel

Request: "Cancel that migration."

Route: `use_existing_job` only when the target job is exact or safely inferred.
If the migration is running, cancellation follows the tracked-work cancel
contract. If the target is ambiguous or jobs are unavailable, `ask_user` before
touching peers or schedules.

## Non-goals

- No runtime implementation.
- No new daemon route, MCP tool, CLI command, dashboard view, or ACP executor.
- No changes to ask reminder, ack, pending reply, or schedule delivery
  semantics.
- No memory file mutation or automatic learning behavior.
- No peer identity, durable restart, state-store, graphify, or Beads ledger
  changes.
