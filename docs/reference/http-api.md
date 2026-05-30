# HTTP API

The daemon exposes HTTP routes for the dashboard, hooks, CLI helpers, and client libraries. The stable public client surface is the CLI, MCP tools, and Python client; raw HTTP routes may move faster.

## Primary route groups

- `/health` and status routes for daemon checks.
- `/peers` for peer registration, listing, lookup, and lifecycle operations. Includes `GET /peers/{id}/doctor` (read-only diagnostic report with contradiction detection) and `POST /peers/{id}/rehook` (non-destructive inbound ws-hook recovery, same-host, dry-run by default).
- `/ask`, `/ack`, and `/asks/pending` for ask lifecycle.
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
- [Operations: architecture](../operations/architecture.md)
