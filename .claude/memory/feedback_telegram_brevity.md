---
name: feedback_telegram_brevity
description: Keep Telegram/notify_peer messages to the human short — phone-readable, a few lines, lead with the point.
metadata:
  type: feedback
---

Messages to the human via `notify_peer` (telegram-claude-code / @telegram) must be Telegram-worthy: short, scannable on a phone, a few lines max. Lead with the point; drop the multi-section walls of bullets.

**Why:** Prass reads these on a phone (2026-05-31 callout). Long structured status dumps that work in a terminal are unreadable on Telegram.

**How to apply:** One headline line + at most a few short lines. Put detail in the PR body / vault / GitHub issue, not the notify. If it needs more than ~5 lines, it belongs somewhere else and the notify should just point at it. Status-roundup style (this-merged, that-cut, here's-what's-left) should be terse, not exhaustive.

**AFK constraint (2026-05-31):** when Prass is on Telegram he is AFK from the terminal — he cannot see `AskUserQuestion` (it renders in the terminal UI). To ask him anything while he's on Telegram, put the actual question + options IN the `notify_peer` message text and ask him to reply (e.g. terse lettered options "reply 1a 2c"). Do NOT use AskUserQuestion to ask a Telegram-present human.
