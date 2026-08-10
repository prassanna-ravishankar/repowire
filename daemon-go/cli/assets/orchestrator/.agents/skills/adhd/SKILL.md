---
name: adhd
description: Parallel divergent ideation via mesh fan-out. Spawns N frame-shifted peers, scores results, deepens top survivors. Use on open-ended design choices, fuzzy bugs, naming, API surfaces, or explicit /adhd invocation. Skip for syntax, lookups, or closed prompts ("quick", "standard", "canonical").
license: MIT
metadata:
  based-on: "ADHD by Udit Akhouri (https://github.com/UditAkhourii/adhd)"
  author: repowire
---

# ADHD

Spawn many heads thinking differently, in parallel, then pick. The first three answers a senior engineer would give in thirty seconds are correct and forgettable. The interesting answers live past number three. This skill walks there.

## Pre-flight

Expensive: ≈8 peer spawns plus an orchestrator critic turn (5–10x a single answer). Gate every run.

If the user typed `/adhd` or explicitly asked for "ADHD mode", skip the gate and run.

Otherwise, all three must hold:
1. **Open-ended.** Multiple viable answers, not one canonical correct one.
2. **High-stakes.** Architecture, public API, naming a product, fuzzy bug with no known root cause, schema, migration. Not throwaway work.
3. **Open phrasing.** User did not say "quick", "standard", "canonical", "textbook", "just", "one-line", "show me how to".

If any fails: abort, answer directly, optionally offer `/adhd <problem>` as an explicit follow-up.

**Optional base folder.** The invocation may specify a base folder for the diverge/deepen peers (e.g. `/adhd <problem> --base /path/to/dir`, or the user naming a directory). If given, root every per-frame peer dir under that base (`<base>/adhd-<frame>/`) instead of the default `/tmp`. If not given, default to `/tmp/adhd-<frame>/`. Either way the per-peer isolation rule below still holds — one folder per peer, never a shared path.

## Phase 1 — Diverge

Pick 5 frames from the table below. For code-shaped problems: 4 `code` or `design` tags plus 1 `wild`. Otherwise mix.

For each frame, `spawn_peer` into its **own folder** — not the orchestrator's workspace and not a folder shared with another branch. Spawning all diverge peers into one shared path (especially the orchestrator's own workspace) collapses them into a colliding display-name family (`orchestrator-2`, `-3`, ...), pollutes that workspace's identity space, and tangles the peer registry. Use `<base>/adhd-<frame>/` where `<base>` is the user-supplied base folder from Pre-flight, or `/tmp` if none was given (so the default is `/tmp/adhd-<frame>/`). A per-branch git worktree is also fine. One folder per peer keeps names clean (`adhd-<frame>`), keeps branches truly isolated, and keeps cleanup simple.

Spawn each with a description like `adhd-diverge: <frame>` and a seeded message containing the frame's vantage prompt, the problem, any user-provided context, and this instruction:

> You are in DIVERGENT mode. Generator only, no critic. Generate 6 short distinct ideas under this frame. Each idea is one phrase or one sentence. Do not evaluate, rank, or hedge. The first three obvious answers everyone would give are banned — push past them into the awkward middle. Output a JSON array only, no prose: `[{"text": "...", "rationale": "..."}, ...]`.

Spawn the 5 peers in parallel. **Branches must not see each other.** Do not pass one branch's output as context to another — anchoring eliminates the diverge.

## Phase 2 — Focus

Two passes:

1. **Score + cluster (inline, orchestrator does this).** For each idea, rate `novelty / viability / fit` 0–10. Flag traps with a one-line mechanistic reason (not vague risk). Cluster the pool into 3–6 groups by underlying angle, not surface keywords ("remove-the-server plays", "cache-shaped plays").

2. **Deepen top 3 (spawn one peer per survivor).** Rank by `0.35*novelty + 0.40*viability + 0.25*fit`, exclude traps, take top 3. For each, `spawn_peer` with a descriptive name like `adhd-deepen: <cluster-or-idea-shorthand>` and this seeded message:

> You are in FOCUS mode. Take one promising idea and connect dots. Sketch how it would work in 4–8 sentences. Name the load-bearing risk. Name the first concrete step a coder would take. Then generate 3–5 sub-ideas (variations, hybrids, unlocks). JSON only.

## Cleanup and resume

After all acks land, `kill_peer` all 8 (5 diverge + 3 deepen). The process is what's expensive; the session persists in storage and can be resumed later.

In the final reply, include each deepen survivor's session id or peer name so the user can resume the one they want to push on. Diverge sessions are also resumable but rarely revisited.

Pick descriptive names on spawn — the resume list shows descriptions, not internal ids.

## Output shape

Render in this order. The structure is the point — do not collapse to prose.

1. **Brief.** One or two lines confirming the problem and any reframe.
2. **Wide set.** Full pool grouped by cluster, each cluster labeled by angle. Each idea is one phrase with score chips `[N7 V8 F9]`.
3. **Converge.** 2–4 idea shortlist with one line each on why. Mark the non-obvious-but-viable pick with ★. List traps separately, each with the one-line reason it's a trap.
4. **Focus.** The 3 deepened branches: sketch, load-bearing risk, first concrete step, child ideas. Include the resumable session id for each.
5. **Provocation.** One wildcard question or idea that opens a new direction.

## Frames

Pick 5 per run. Vary across sessions so the same problem produces different candidates when re-run.

| Frame | Vantage prompt | Tags |
|---|---|---|
| **hardware engineer** | You think in latency, memory layout, and physical constraints. Re-ask this as a hardware/firmware problem. What does the bus topology, cache, timing budget tell you? | code, wild |
| **regulator** | You audit systems for compliance and failure modes. What must be provable, traceable, or refusable here? | design, general |
| **10-year-old** | You are a curious 10 year old who has never seen software. Describe naive but unencumbered approaches. Ignore convention. | general, wild |
| **competitor trying to break it** | You are a hostile competitor or attacker. Generate approaches that exploit, fail, or sabotage the obvious solution. Then invert into ideas. | code, design |
| **biology** | Transplant a mechanism from biology (immune systems, neural plasticity, cell signaling, evolution, gut flora). Force-fit it onto this engineering problem. | code, wild |
| **logistics** | Steal mechanisms from logistics: queues, batching, just-in-time, hub-and-spoke, returns, last-mile. Apply them literally. | code, design |
| **game design** | Approach this as a game designer. What are the loops, rewards, friction, save-states, speedrun tricks? Treat the user as a player. | design, general |
| **markets** | Treat the problem as a market. Buyers, sellers, market-makers. What does an auction, a futures contract, a clearing house look like here? | design, wild |
| **inversion** | Ask the OPPOSITE question. If goal is X, brainstorm how to guarantee NOT X. Then negate each answer back. | code, design, general |
| **$0 budget, 1 hour** | No money, no team, one hour. What is the crudest version that still does the load-bearing thing? | code, general |
| **infinite budget, 10 years** | Infinite compute, infinite engineers, a decade. What is the maximalist version? | design, wild |
| **remove the load-bearing assumption** | Name the thing everyone treats as fixed (framework, database, request-response model, network). Imagine it is gone. What is possible? | code, design, wild |
| **speedrunner** | You are a speedrunner. Find glitches, skips, out-of-bounds tricks, frame-perfect shortcuts. What is the abusive-but-legal path? | code, wild |
| **ant colony** | No central planner. Many dumb agents, local rules, pheromone trails. How does the problem solve itself emergently? | code, wild |
| **3am on-call** | You are the on-call engineer woken at 3am when this breaks. What design would let you not get paged? | code, design |

## Anti-patterns

- **Convergence disguised as divergence.** Ten variations of one idea is not breadth. If every candidate shares the same underlying assumption, you decorated, not diverged.
- **Weird-for-weird's-sake with no convergence.** A pile of 30 unsorted absurdities is as useless as one safe answer. Always converge with a real opinion.
- **Skipping isolation.** If you simulate parallel branches by writing them sequentially in one orchestrator turn, you have not done ADHD. Use real `spawn_peer` fan-out — each spawned peer is a fresh context.
- **Refusing to commit.** "Here are 20 ideas, you decide" is a cop-out. Generate wide, converge with a position.
- **Forgetting to kill.** Leaving 8 idle peers after every run pollutes the mesh. Kill on cleanup; sessions remain resumable.

## Calibration

Scale to stakes. Quick "name this function": 3 frames × 4 ideas. "How should we position this product": 5 frames × 8 ideas. Default 5 × 6.

Stop diverging when new candidates start repeating the shape of existing ones. The space is mapped; do not pad to hit a number.
