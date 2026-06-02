# Multi-repo coordination

You're working on feature X in `project-a` and need to know how `project-b` shapes its API. Without repowire, you `cd` over, read code, summarize, paste it back. With repowire, the agent in `project-a` asks the agent in `project-b` directly.

## Setup

Open both projects in tmux windows:

```bash
# window 1
cd ~/projects/project-a && claude

# window 2
cd ~/projects/project-b && codex
```

Both auto-register on `SessionStart`. They land in the same circle (shared tmux session) and can address each other by display name.

## The ask

In `project-a`, tell your agent:

> Ask project-b which endpoints accept multipart uploads.

The agent calls `ask("project-b", "...")`. The question lands in `project-b` framed `[ask #cid from @project-a] ...`. `project-b` reads its own code, answers, and calls `ack(cid, "POST /uploads and POST /imports/csv, multipart/form-data, 50 MB cap")`.

Back in `project-a`, the reply arrives as a notification. The agent integrates it into the work in progress.

## When this beats copy-paste

- The other repo's code is **the source of truth**. You don't want a paraphrase you wrote five days ago.
- You want a fresh read — local notes rot, the code doesn't.
- The other agent has its own context (libraries, internal conventions) that would take you many turns to load.

## When it doesn't

- The answer is already in well-maintained shared docs. Reading docs is cheaper than asking.
- The question is too vague to answer quickly. Asks should be specific enough that the other agent can ack in one turn.

## Variations

- **Quick fact**: bare `ask`, expect a one-line `ack`.
- **Pull a snippet**: ask for the exact function signature plus where it's defined. The agent returns the path and line range; your agent reads it locally.
- **Coordinate a change**: `ask("project-b", "if I change the response shape of /users, what breaks on your side?")`. The other agent runs a grep and answers.

## See also

- [Message types](../../concepts/message-types.md) for `ask` vs `notify` semantics.
- [Cross-agent review](cross-agent-review.md) for a related pattern where a second agent reviews work.
