---
name: memory
description: Use when deciding whether to write, read, update, consolidate, or avoid orchestrator memory; distinguishing memory from board status, job state, session history, or project-local agent context.
---

# Memory

Memory is for durable operational lessons: "next time X comes up, do Y differently." It is not status, history, transcript recall, or job state.

Use Repowire memory for orchestrator lessons:

```bash
repowire memory list --scope orchestrator
repowire memory show <slug> --scope orchestrator
repowire memory search <query> --scope orchestrator
repowire memory write <slug> --scope orchestrator --body "..." --type lesson --description "..."
```

## What belongs where

- Memory: forward-applicable operating lessons and stable preferences.
- Board: owner, state, blocker, next action.
- Jobs: attempts, lifecycle state, failures, results.
- Session history/search: detailed recall and evidence.
- Project agent files: context peers in that project should always load.

Other peers do not inherit orchestrator memory implicitly. Include relevant memory context in briefs when it materially changes the work.

## Write rules

Write after an explicit durable-memory decision or a clear correction that should change future behavior. Keep each memory focused and short. If two memories overlap, consolidate. If a memory becomes stale, update it rather than adding a competing rule.

Do not save one-off events, activity counts, recent work summaries, or full histories as memory.
