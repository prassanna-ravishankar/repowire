---
name: delegation
description: Use when handing work to another peer, deciding whether to spawn or reuse a peer, briefing a lane, asking for a plan before code, or using a second peer for critique or review.
---

# Delegation

Use this when giving work to a peer.

## Spawn or reuse

Reuse a peer when the work continues its existing context in the same worktree. Spawn fresh for independent review, decoupled concerns, cross-model critique, or when the current peer's context is too long.

One worktree per concern. Do not put two implementation peers on overlapping files in the same worktree.

## Brief shape

Calibrate brief depth to the cost of getting it wrong.

- Small fix: one sentence plus file/line.
- Bug fix: repro, file/line hints, and verification.
- Feature work: success criteria, relevant files, docs impact, and reporting surface.
- Architecture: plan-before-code, risks to probe, expected output shape, and citations.

Always include what success looks like and how the peer should report back.

## Plan before code

Ask for a plan before code when a wrong approach would cost more than about 30 minutes to unwind. For architectural-but-bounded plans, use a different-model peer to critique before implementation.

Critique brief: include the proposed plan, files to inspect, risks to probe, and ask for skeptical-but-fair feedback with citations. Relay the critique back to the implementing peer and ask them to absorb or push back with reasoning.

## Review before merge

Use a different peer for review when stakes warrant. CI green is not feature verification; smoke the actual behavior for UI, API, or workflow changes.
