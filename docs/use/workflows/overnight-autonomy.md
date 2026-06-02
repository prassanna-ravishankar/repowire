# Overnight autonomy

Give peers tasks, close your laptop lid, walk away. Peers work autonomously and report back via Telegram or the dashboard when you come back. Long-running tasks (migrations, refactors, exhaustive test suites) complete while you sleep.

## What makes this work

Three properties of repowire combine to make overnight work tractable:

1. **Lazy repair, no polling.** Idle peers do not burn CPU or rate limits. A peer that finished in the first hour sits at `online` cheaply until the next event.
2. **Persistent registration.** Peers and their state persist across daemon restarts. If your laptop sleeps and reconnects, the registry survives.
3. **Telegram delivery.** `notify_peer("telegram", "...")` reaches you regardless of whether you're at the machine. Push notifications wake you only when something needs you.

## Setup

Before walking away:

```text
@orchestrator: run the v1 → v2 migration on project-a overnight.
  Notify @telegram at each major step. Notify @telegram on completion
  or any blocker. Don't push to main without my sign-off.
```

The orchestrator dispatches to `project-a`, watches for progress, and forwards updates to your phone. If anything stalls, you get a push notification — and you can reply from your phone via the Telegram bot.

## Schedules as guardrails

If you want a hard deadline:

```python
schedule_create(
    to_peer="orchestrator",
    text="status check: where is the v1->v2 migration?",
    fire_at="2026-05-15T06:00:00Z",
    kind="ask",
)
```

At 6 AM the orchestrator gets a forced ask; the answer surfaces in your morning Telegram (or whatever surface the orchestrator chooses to forward to). For a recurring cadence, use `schedule_cron` or `schedule_self(..., cron="...")`:

```python
schedule_cron(
    to_peer="orchestrator",
    text="weekday morning status: overnight work, blockers, next action",
    cron="0 6 * * 1-5",
    kind="ask",
)
```

## Failure modes to plan for

- **Auth tokens expire.** Long-running sessions that depend on cloud credentials may hit a token refresh window. Confirm tokens before you walk away; or have the peer surface a clear `notify_peer("telegram", "auth failed")` when it can't proceed.
- **Rate limits.** A peer hitting a credit cap or a per-minute limit will stall. Cross-runtime pairing (claude-code orchestrator with a codex worker) sidesteps this for some workloads.
- **`turn_state=awaiting_input`.** A peer mid-permission-prompt is stuck waiting for a human. Either pre-approve the tools it will need, or accept that you'll see a Telegram push and respond.
- **Network blips.** The daemon's relay client auto-reconnects, but you'll see brief offline windows. Telegram delivery resumes when the relay does.

## When this is the right tool

- Migrations and bulk refactors with a clear definition of done.
- Test-suite runs and flaky-test bisection.
- Documentation generation against a stable surface.
- Exploratory work where you're happy with whatever the agent produced overnight.

## When it isn't

- Anything that requires constant judgment calls.
- Tasks where every step has a permission prompt you haven't pre-approved.
- Work where you'd rather see incremental results and steer mid-stream.

## See also

- [Schedules](../../reference/mcp-tools.md#scheduling) — `schedule_create`, `schedule_self`, `schedule_cron`, `schedule_list`, `schedule_delete`.
- [Mobile mesh management](mobile-mesh.md) — talking to peers from your phone.
- [Troubleshooting: ghost peers](../../troubleshooting/ghost-peers.md) for stuck-state diagnosis.
