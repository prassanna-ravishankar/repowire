# HTTP API

The daemon exposes HTTP routes for the dashboard, hooks, CLI helpers, and client libraries. The stable public client surface is the CLI, MCP tools, and Python client; raw HTTP routes may move faster.

## Primary route groups

- `/health` and status routes for daemon checks.
- `/peers` for peer registration, listing, lookup, and lifecycle operations. Peer records include optional `model`, the last observed runtime model reported by the backend. Includes `GET /peers/{id}/doctor` (read-only diagnostic report with contradiction detection) and `POST /peers/{id}/rehook` (non-destructive inbound ws-hook recovery, same-host, dry-run by default).
- `POST /peers/{id}/offline` accepts an optional body `{reason, source, detail, terminal}` recording the truthful cause in the event log (e.g. `session_end`, `agent_exited`, `pane_takeover`). `terminal: true` retires the peer: its websocket is severed and reconnects claiming that peer_id are rejected (HTTP 409 / ws error `peer_retired`) unless they carry a live `agent_pid`. An empty body keeps the legacy plain-offline behavior.
- `GET /panes/orphans` lists local tmux panes not bound to any registered peer (with a display-only backend hint); `POST /panes/{pane_id}/link` adopts one — registers the peer AND establishes its inbound ws-hook, rolling the registration back if no live transport connects (fail-closed against ghosts). Driven by `repowire link`.
- `/ask`, `/ack`, and `/asks/pending` for ask lifecycle.
- `/answer` records a typed answer to a structured question; `POST /questions/ask-blocking` registers a blocking structured question (tool permission) and holds the connection open until it is answered or the daemon's wait cap elapses, then returns the typed answer (fail-closed: a tool-permission question denies on timeout). Used by blocking transports such as the ACP broker and the Claude Code `PreToolUse` approval hook.
- `/query` is a legacy blocking compatibility route. It opens a structured text ask through the ask lifecycle, waits for the recipient's answer/ack, and returns the old `{text, error, status}` response shape for existing clients.
- `/traces/{trace_id}` returns the recorded delivery stages for an ask (`correlation_id`) or notify (`delivery_id`) from the local delivery trace ledger.
- `/messages` and WebSocket routes for live delivery.
- `/schedules` for one-shot and recurring scheduled messages.
- `/jobs` / work routes for durable tracked work.
- `/attachments` for upload and download.
- `/dashboard` for the static dashboard bundle.

## Jobs Execution Policy

`POST /jobs` accepts `process_scope` and `continuity` for path/backend durable
jobs. Unassigned path/backend jobs default to `process_scope=per_fire`, so each
run uses a short-lived executor process that is released after terminal
completion. One-shot jobs default to `continuity=fresh`; recurring jobs default
to `continuity=resume`, so the next fire resumes backend-native runtime context
when a runtime session id is available. Use `continuity=fresh` to avoid backend
resume.

## Jobs List Views

`GET /jobs` returns the full durable-work list by default. Dashboard-style clients that only need row data can use:

```http
GET /jobs?view=summary
```

The summary view keeps the same `{ "work": [...], "recurring": [...] }` envelope and preserves ids, state, timestamps, ownership/routing fields, result summaries, and trimmed execution target/delivery metadata. It omits heavier detail fields such as full requests, provenance, runner state, prompt bodies, and progress history. Fetch `GET /jobs/{id}/status` for the selected job or recurring `cal-*` template when full detail is needed.

## Auth

When `daemon.auth_token` is configured, clients send:

```http
Authorization: Bearer <token>
```

## Related

- [Python client](python-client.md)
- [CLI](cli.md)
- [Operations: architecture](../operate/architecture.md)
