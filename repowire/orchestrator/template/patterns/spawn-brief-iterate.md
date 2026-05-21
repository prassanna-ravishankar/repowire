---
name: spawn-brief-iterate
description: Default dispatch loop for new work handed to a peer.
triggers: [new-work, peer-dispatch, review]
risk: medium
surfaces: [mcp, dashboard, telegram]
---

# Pattern: spawn → brief → iterate

The default dispatch loop. Use this for any new piece of work you're handing to a peer.

## When to reach for it

Any time you're about to dispatch work. This is the baseline shape; other patterns are specializations.

## Shape

1. **Spawn or reuse a peer.**
   - Default: reuse if the work is continuous with their prior turn (same worktree, same concern).
   - Spawn fresh when: independent review needed, cross-model critique needed, decoupled concern, or the peer's context is already too long.
   - MCP signature: `spawn_peer(path, backend, circle="default", message=None)`. The `backend` carries the runtime choice — match to the work shape:
     - **pi**: `"pi"` — orchestrator-shaped, conversational
     - **codex**: `"codex --dangerously-bypass-approvals-and-sandbox"` — bare codex blocks on approval prompts during warmup
     - **claude-code**: `"claude --dangerously-skip-permissions"`
     - **gemini**: `"gemini --yolo"`
     - **opencode**: `"opencode"`
   - The backend must be configured in `daemon.spawn.commands` in `~/.repowire/config.yaml` or spawn is rejected. `circle` maps to the tmux session name and can't be reassigned after spawn. `message` seeds first-turn context (task brief); required for codex peers (others ignore it).

2. **Brief with memory refs and stakes calibration.**
   - **Typo / one-line fix:** one-sentence brief. "Fix X at file:line per Y."
   - **Bug fix:** brief + file:line citations + what to verify after.
   - **Feature work:** brief + relevant memory files (`memory/<topic>.md` if any) + pattern references.
   - **Architectural plan:** long brief with full context, file:line citations, expected output shape, risk categories to probe, tone instruction ("skeptical-but-fair, don't be a yes-man").
   - Always include: how to report back (notify / ack), where their output should land (file in worktree or message back), what success looks like.

3. **Iterate before code.**
   - Trigger rule: **ask for plan-before-code when the wrong approach would cost >30 min of rework.** Vague "non-trivial" thresholds get skipped; the 30-min-rework rule is the decision cue you can actually hold against the work in front of you.
   - For architectural-but-bounded plans (auth flows, framework boundaries, multi-layer changes), run `patterns/two-model-critique.md` here.
   - This is the most-violated step in dispatch. Skipping it is how plan-shaped mistakes become PR-shaped mistakes.

4. **Review before merge.**
   - PR review by a different peer (cross-model when stakes warrant) before squash-merge.
   - Resolve threads as part of the flow, not after.

5. **Verify after.**
   - Smoke test the actual feature, not just CI green. UI changes need a browser check. API changes need a curl + response inspection. CI tests verify code correctness, not feature correctness.

## Anti-patterns

- **Cold dispatch.** Spawning a peer with "fix this bug" and a link, no context, no memory refs. Cost: the peer re-derives context you already had, slowly.
- **No brief calibration.** Long brief for typo fixes wastes everyone's time; short brief for architectural work guarantees rework.
- **Skipping iterate-before-code.** Lets a plan-shaped mistake become a PR-shaped mistake. Cheap to correct a plan; expensive to correct a PR.
- **Trusting CI green as feature-verified.** Tests verify code correctness, not feature correctness.

## Calibration cue

Brief length should be roughly proportional to *stakes of getting it wrong*, not size of the change. A 5-line fix in customer-facing code warrants more brief than a 200-line internal refactor.
