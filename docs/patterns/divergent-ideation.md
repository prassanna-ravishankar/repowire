# Divergent ideation

When the obvious answer to a design choice would be competent and forgettable, the orchestrator can run a structured brainstorm across the mesh instead of answering directly. Five frame-shifted peers diverge in parallel, the orchestrator scores and clusters their output, three deepen peers expand the survivors, and the user picks one to keep iterating on. The shipped `adhd` skill (`.agents/skills/adhd/SKILL.md`) teaches the orchestrator the full loop; this page is the operator-facing how-to.

Reach for it on open-ended design choices, fuzzy bugs with no known root cause, public API or naming decisions, and any prompt of the shape *"give me a few ways to…"*. Skip it for syntax lookups, known root causes, or prompts phrased closed ("quick", "standard", "canonical").

## Pre-flight gate

This is expensive — about ten LLM/agent calls per run, five to ten times the cost of a single answer. The skill self-judges before running:

- **Explicit invocation.** If the user typed `/adhd` or asked for "ADHD mode", skip the gate.
- **Self-judge.** Otherwise, all three must hold: the answer space is open-ended (multiple viable answers), the cost of the obvious answer being wrong is high, and the user's phrasing is open. If any fails, the orchestrator answers directly and optionally offers `/adhd <problem>` as a follow-up.

The gate exists because every run spawns eight peers. Without it, the orchestrator will over-trigger and pollute the mesh.

## The loop

### Phase 1 — Diverge

Pick five cognitive frames from the catalog (hardware engineer, regulator, 10-year-old, biology, speedrunner, and so on — see the full table in `.agents/skills/adhd/SKILL.md`). For each frame, the orchestrator spawns one peer in parallel:

```text
spawn_peer(
  path=cwd,
  backend="claude-code",
  message="<frame vantage prompt>\n\n<problem>\n\n<context>\n\n"
          "You are in DIVERGENT mode. Generator only, no critic. "
          "Generate 6 short distinct ideas under this frame. "
          "The first three obvious answers everyone would give are banned. "
          "Output a JSON array only.",
)
set_description("adhd-diverge: hardware-engineer")
```

The peers must run in parallel with no cross-context. If one branch sees another's output, the diverge collapses to a wider single thought. Each frame produces six short ideas; the pool is roughly thirty candidates.

### Phase 2 — Focus

The orchestrator does the critic work itself, in its own turn:

1. **Score** every idea on `novelty / viability / fit` (0–10 each).
2. **Cluster** the pool into three to six groups by underlying angle, not surface keywords ("remove-the-server plays", "cache-shaped plays"). Surface the *shape* of the design space, not the keywords.
3. **Flag traps** with a one-line mechanistic reason ("shelve isn't thread-safe under multi-writer load"), not a vague risk label.

Then rank by `0.35*novelty + 0.40*viability + 0.25*fit`, exclude traps, take the top three. For each, spawn one focused peer with a descriptive name:

```text
spawn_peer(
  path=cwd,
  backend="claude-code",
  message="<top-3 idea>\n\n"
          "You are in FOCUS mode. Sketch how this would work in 4-8 sentences. "
          "Name the load-bearing risk. Name the first concrete step. "
          "Generate 3-5 sub-ideas (variations, hybrids, unlocks). JSON only.",
)
set_description("adhd-deepen: cache-shaped")
```

### Cleanup and resume

Once all eight acks land, `kill_peer` every spawned peer. The process is what's expensive (tmux pane, CLI runtime); the session persists in storage and can be resumed. The orchestrator's final reply names each deepen survivor's session id so the user can resume the one they want to push on:

> Focus branch *cache-shaped* — session `repow-default-7d31052e`. Resume with `repowire peer resume repow-default-7d31052e` to keep iterating on it.

Diverge sessions are also resumable but rarely revisited.

## Frames

Frames are vantage-prompt rewrites that re-pose the whole question — *"re-ask this as a hardware problem"*, *"re-ask this as a regulator"*, *"re-ask this as a 10-year-old"*. They are the load-bearing mechanism: branches diverge because the *vantage* differs, not because the next token differs.

The shipped catalog has fifteen frames tagged `code`, `design`, `general`, and `wild`. For code-shaped problems, the skill picks four code/design frames plus one wild. See `.agents/skills/adhd/SKILL.md` for the full table.

## Output shape

The structure is the point. The orchestrator renders, in order:

1. **Brief** — one or two lines confirming the problem and any reframe.
2. **Wide set** — full pool grouped by cluster, score chips `[N7 V8 F9]` next to each idea.
3. **Converge** — two-to-four-idea shortlist with one line each on why. The non-obvious-but-viable pick marked with ★. Traps listed separately with their one-line reasons.
4. **Focus** — the three deepened branches: sketch, load-bearing risk, first concrete step, child ideas. Resumable session id for each.
5. **Provocation** — one wildcard question that opens a new direction.

Do not collapse to prose. The clusters and the shortlist are the value.

## Cost

Five diverge + one score-cluster turn + three deepen ≈ ten LLM/agent calls per run. Roughly five to ten times a single-shot answer. Not for inner-loop work; gate every run on the pre-flight check.

## When to skip

- Factual lookups, syntax help, anything one search query away.
- Bugs with a known root cause.
- Prompts phrased closed: *"quick"*, *"standard"*, *"canonical"*, *"textbook"*, *"just show me"*, *"one-line"*.
- Per-keystroke work where the cost of the obvious answer is low.

When in doubt, answer directly and offer `/adhd <problem>` as an explicit follow-up.

## See also

- Skill source: `.agents/skills/adhd/SKILL.md` in the orchestrator workspace.
- Upstream ADHD project: [github.com/UditAkhourii/adhd](https://github.com/UditAkhourii/adhd) (MIT). The two-phase loop and frame catalog are from there; the mesh-side execution is repowire-specific.
- [`spawn_peer`](../reference/mcp-tools.md#spawn_peer), [`kill_peer`](../reference/mcp-tools.md#kill_peer), [`ask`](../reference/mcp-tools.md#ask) — the primitives this pattern composes.
- [Orchestrator coordination](orchestrator-coordination.md) — the broader dispatch loop this pattern slots into.
