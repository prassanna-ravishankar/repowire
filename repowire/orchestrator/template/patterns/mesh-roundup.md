---
name: mesh-roundup
description: Poll several peers for status and compile an impact-first update.
triggers: [status-sweep, handoff, morning-review, evening-review]
risk: low
surfaces: [mcp, dashboard, telegram]
---

# Pattern: mesh roundup

Poll N peers for status in parallel; compile impact-first.

## When to reach for it

- User asks "what's everyone working on?" / "where are we?"
- End of session and you want to leave handoff state
- Evening / morning sweep to surface blockers

## When NOT to reach for it

- Single-peer status check — just `ask()` them
- When you have no specific question — vague "how's it going?" gets vague answers back

## Shape

1. **`list_peers()`** to enumerate online peers.

2. **Decide what to ask each peer.** The question shapes the value of the roundup. Bad: "status?" Good: "what's blocking, what's the next merge, anything you need from me?"

3. **Send `ask()` to all relevant peers in parallel**, not sequentially. Collect correlation_ids.

4. **Wait for ack-replies** to land in your inbox. Give it ~30-60s for a normal roundup; reminders fire after a one-turn grace if peers are slow.

5. **Compile impact-first.** Lead with the highest-impact item (a blocker, a PR ready to merge, a regression caught). Group small consistency PRs into one mention. Don't open or close with counts ("N PRs shipped, M issues closed") — that's score-keeping framing, drifts performative.

6. **Format for the user's primary channel.** If user is on telegram (phone), this MUST be short — one paragraph max, 2-3 lines, lead with the one most-blocking item. Save structured roundups for the dashboard.

## Anti-patterns

- **Count-leading summaries.** "9 PRs shipped, 3 issues closed today." Reward-volume framing. Replace with impact.
- **Score-keeping drift.** Don't track PR/issue tallies session-over-session; the user notices and pushes back.
- **Waterfall asks.** Sequential `ask()` calls turn a 30s roundup into a 5min roundup. Parallel always.
- **Wall-of-text on telegram.** Per-peer sections with emojis on a phone screen are unreadable; collapse to one paragraph or send only the load-bearing item.
