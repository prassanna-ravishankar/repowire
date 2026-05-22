# Mesh command UX contract

This contract defines the user-facing command layer Repowire should expose for
mesh and session operations. It is a design contract, not a new daemon job
store. Current implementations may be CLI commands, MCP tools, dashboard
actions, Telegram or Slack commands, or future Claude Code plugin commands.

The command layer must keep two renderings for every command:

- **Human rendering**: compact text or UI for a person steering the mesh.
- **JSON rendering**: stable structured output for agents, plugins, tests, and
  scripts.

## Command set

| Command | Purpose | Backing surface today |
| --- | --- | --- |
| `status` | Summarize daemon reachability, installed transports, online peers, and current peer identity. | `repowire status`, `GET /health`, `GET /peers`, `whoami` |
| `peers` | List addressable peers with id, name, circle, role, status, turn state, backend, and description. | `repowire peer list`, MCP `list_peers`, `GET /peers` |
| `pending-asks` | Show open inbound and outbound ask threads for a peer or session. | `GET /asks/pending?direction=both`, `repowire peer describe` |
| `ask` | Open a tracked, non-blocking ask thread that the recipient must close with `ack`. | MCP `ask`, `POST /ask` |
| `notify` | Send a fire-and-forget message with no expected response. | MCP `notify_peer`, `POST /notify` |
| `schedule` | Create, list, or delete one-shot and recurring future mesh messages. | `repowire schedule`, MCP schedule tools, `/schedules` |
| `timeline` | Read a peer or session timeline from realtime events plus available transcript history. | `/events`, `/peers/{id}/timeline`, session timeline routes |
| `result` | Read the latest visible outcome for a thread, schedule delivery, or session turn. | ask tracker state, events, session timeline |
| `doctor` | Diagnose setup, transport, daemon, relay, and state-store health. | `repowire doctor`, `repowire.doctor` |

Optional review controls can be layered on top of the same rendering rules, but
they are not part of the minimum command set for this contract.

## Invocation boundaries

Commands must name their routing scope explicitly when there is any chance of
ambiguity:

- Use `peer_id` for exact routing. Display names are human-facing and can
  collide across circles.
- Use `circle` when addressing a display name outside the caller's default
  circle.
- Use `repowire_session_id` only for session-scoped commands. If the session has
  no live executor, commands must report that state instead of silently falling
  back to a peer.
- Human surfaces may accept short slash forms such as `/peers` or `/ask`, but
  the resolved command must still map to one command id from the table above.

Agents must not use `SendMessage` for Repowire peers. `SendMessage` is only for
same-session harness teammates. Mesh peers are reached with Repowire commands:
`ask` for tracked work, `notify` for fire-and-forget updates, and `ack` to close
an inbound ask.

## Ask, ack, and notify rules

The command UX must preserve the lifecycle distinction:

- `ask` is non-blocking and returns a `correlation_id` immediately. The returned
  id is not a delivery receipt.
- A recipient closes an ask with bare `ack(correlation_id)` when no substantive
  reply is needed.
- A recipient replies with `ack(correlation_id, message)`. The reply is the
  close operation and is delivered back to the original asker.
- Follow-ups use `ask(reply_to=correlation_id, ...)`, which closes the prior
  thread and opens a new tracked thread.
- `notify` is fire-and-forget. It may return a local notification id for
  observability, but that id is not an ask thread and cannot be acked.
- `schedule --kind ask` opens an ask thread when the schedule fires.
  `schedule --kind notify` delivers a fire-and-forget notification.

## JSON rendering

Every command that returns data should support this envelope:

```json
{
  "command": "peers",
  "status": "ok",
  "schema_version": 1,
  "target": {
    "peer_id": "repow-5-abd4d21e",
    "peer_name": "project-a-codex",
    "circle": "default",
    "repowire_session_id": "rws-..."
  },
  "data": {},
  "warnings": [],
  "next_actions": []
}
```

Required fields:

- `command`: one command id from the command set.
- `status`: `ok`, `empty`, `partial`, or `error`.
- `schema_version`: integer, starting at `1`.
- `data`: command-specific object or list.

Optional fields:

- `target`: resolved peer or session identity.
- `warnings`: non-fatal issues, such as stale peer state or partial transcript
  history.
- `next_actions`: short machine-readable hints such as `ack`, `retry`,
  `choose-circle`, `start-daemon`, or `run-doctor`.

Command-specific `data` objects should prefer existing daemon field names:
`peer_id`, `display_name`, `circle`, `role`, `status`, `turn_state`, `backend`,
`description`, `correlation_id`, `schedule_id`, `kind`, `fire_at`, `cron`,
`repowire_session_id`, and `event_id`.

## Human rendering

Human output should be brief and scannable:

- `status`: one daemon line, one install/transport line, one peer-count line,
  and current identity when known.
- `peers`: table columns `name`, `status`, `turn`, `circle`, `role`, `backend`,
  `description`; include `peer_id` on demand or when names are ambiguous.
- `pending-asks`: group inbound and outbound separately; include age,
  `correlation_id`, from/to peer, and a one-line text preview.
- `ask`: print the new `correlation_id` and make clear that the command is
  non-blocking.
- `notify`: print the notification id only as observability, not as a thread to
  wait on.
- `schedule`: print schedule id, kind, target, and next fire time; list mode
  should group one-shot and recurring entries when possible.
- `timeline`: show newest relevant turns/events first by default, with source
  labels for realtime event, transcript history, schedule, or ask.
- `result`: show the latest known answer, completion, or failure. If durable
  tracked work is not available, say which existing source was inspected.
- `doctor`: keep the existing ok/warn/fail/skip hierarchy and add suggested next
  actions only when they are concrete.

## Timeline and result boundaries

`timeline` and `result` are views over existing peer, ask, schedule, event, and
session-history data. They must not imply a daemon-backed job lifecycle unless
the [tracked-work lifecycle](tracked-work-lifecycle.md) is implemented
separately.

Until durable tracked work exists:

- `result` for an ask can report open, acked, or missing ask state.
- `result` for a schedule can report configured, deleted, last delivery event,
  or unknown.
- `result` for a session can report the latest visible chat turn or event, not a
  guaranteed task completion record.

## Doctor and health boundaries

`doctor` may report current daemon, hook, MCP, plugin, relay, scheduler, and
state-store diagnostics. ACP/channel broker readiness, last broker error,
permission relay health, and degradation matrices belong to the ACP/channel
health work. This command contract can reserve fields for them, but should not
claim those states are implemented.

## Plugin packaging boundary

Future Claude Code plugin packaging may ship these commands as slash commands,
skills, MCP bootstrap, hook documentation, or documentation. Packaging must
consume this contract rather than redefine command semantics. Repowire setup
remains the core daemon and multi-runtime install path; a plugin is a
convenience package, not a replacement for setup.

A plugin manifest should map every packaged command to one command id from this
contract and should check drift against the installed Repowire package, MCP
bootstrap command, hook snippets, Claude Code version, and declared compatible
Repowire version range. The detailed packaging boundary lives in
[Claude Code plugin packaging](claude-code-plugin-packaging.md).
