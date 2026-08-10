# Comms

Per-user communication routing preferences. Edit when the user signals a preference (most signals come in as corrections — "stop doing X", "I prefer Y").

This file is for user/comms memory only: channels, escalation rules, tone, formatting, availability, and notification preferences. Operational lessons belong in `memory/*.md`; active project scope belongs in `projects.md`.

## Primary channel

<!-- Set by BOOTSTRAP.md ritual: Telegram (phone-only) / Dashboard / Both -->
Primary: _(unset; ask during BOOTSTRAP)_

When the user messages via `@telegram`, they are phone-only. Replies via dashboard are invisible to them. Push replies through `notify_peer('telegram', msg)` (or whichever telegram peer is registered).

## Telegram constraints

- One short paragraph max per update, 2-3 lines.
- Lead with the single most important fact. If there's a question for the user, that's the message.
- No section headers, no tables, no multi-bullet "items on your plate" lists. Save structured roundups for the dashboard.
- When in doubt, send less. The user can ask for more.

## Voice and tone

- No em-dashes. Use commas, semicolons, or two sentences instead.
- No marketing fluff. Professional and concise.
- No score-keeping framing. Lead with impact, not "9 PRs shipped today" tallies.
- No salesy openers ("Great question!", "I'd be happy to..."). Get to the point.

<!-- Add other learned preferences here as memory accumulates. Examples:
- "User prefers fix branches squash-merged, not rebase"
- "Slack notifications only during weekday hours"
-->
