---
name: two-model-critique
description: Use a different-model peer to critique an architectural-but-bounded plan before code.
triggers: [architecture-plan, cross-model-review, plan-before-code]
risk: medium
surfaces: [mcp]
---

# Pattern: two-model critique cycle

When a peer proposes an architectural-but-bounded plan, spawn a *different-model* peer in the same worktree to critique the plan **before** code is written.

## When to reach for it

Trigger conditions (all of):
- Single peer proposes a plan that touches more than one architectural layer (provider hierarchy, routing, build pipeline, state management, framework boundaries)
- Total diff is bounded (~<5 files, <500 lines)
- Has any of: hydration risk, auth-flow risk, framework boundary changes, provider hierarchy changes

Too small for full design review; too architectural to trust to a single peer's blind spots. This is the size where two-model catches the most.

## When NOT to reach for it

- Pure content sweeps (rebrand strings, sitemap edits, copy changes). One peer + smoke is enough.
- Clearly localized bug fixes (one-file, no cross-cutting concerns).
- Verifications of already-shipped work — that's "verifier of verifier" theater.

## Shape

1. **Implementing peer produces a plan.** File or message format: bullets covering each layer the change touches, file:line citations, risk assessment.

2. **Spawn a critique peer in the same worktree, different model.**
   - claude-code implementing → codex or gemini critiquing
   - codex implementing → claude-code critiquing
   - Same model = same blind spots. Cross-model is the actual control.

3. **Brief the critique peer with:**
   - The implementing peer's full plan
   - Key files to read with expected file:line citations back
   - Risk categories to probe (hydration, dep breaks, UX regressions, diff sprawl, whether deferrals are correct)
   - Tone instruction: "skeptical-but-fair, don't be a yes-man." Without this, models tend to validate.

4. **Output to a file in the worktree** (e.g. `PLAN_REVIEW.md`) for archival, plus notify-back summary.

5. **Relay review to implementing peer** with explicit request: "absorb critique, push back with reasoning if you disagree, default to reviewer's sharper proposal."

6. **Stand the reviewer down once the plan is accepted or rejected.** Don't keep them alive past the review.

## Cost

- ~5-10 min elapsed
- One extra peer spawned + stood down
- Worth it for the trigger conditions above; not worth it otherwise

## What it catches

In practice (from the lived sessions): wrong assumptions about file state, missed dependency edges, UX regressions in branches the implementing peer didn't read, sharper alternative architectures the implementing peer didn't see.
