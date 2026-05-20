---
name: release-bundle-decision
description: Decide whether merged main work should be tagged now or held for a bundle.
triggers: [release, tag-decision, changelog]
risk: high
surfaces: [cli, github]
---

# Pattern: release bundle decision

Given N merged commits on main, decide tag-now-or-hold + version + changelog shape.

## When to reach for it

- A PR just merged and CI is green
- Main has accumulated multiple unreleased commits and the user asks "are we ready to release?"
- A user-facing fix just landed and you're deciding whether to tag immediately

## Shape

1. **Inventory unreleased commits.** `git log <last-tag>..main --oneline`. Note what each commit changes.

2. **Decide tag-now or hold.** Tag if any of:
   - User explicitly called for it
   - An urgent fix needs to reach users via pip / npm / equivalent
   - A coherent milestone is reached (feature complete, refactor done)
   - Multiple bundled-related PRs have accumulated and the bundle itself is the release

   Hold (default) if:
   - The merge was internal / tooling / docs
   - Bundling makes sense (more work coming in the next day or two)
   - A parallel review is still pending against merged-main (see "Never tag from a branch" in AGENTS.md)

3. **If tagging, decide the version.** Semver judgment:
   - **patch** — bug fixes, cleanup, small additions
   - **minor** — significant new features, breaking-but-internal changes
   - **major** (1.0+) — intentional decision, never auto-incremented; ask the user

   When in doubt, **ask the user**. Patch vs. minor isn't always obvious; cheap to clarify.

4. **Draft changelog.** Group by impact, not by PR count. Lead with user-visible changes. Keep tight.

5. **Tag from main only, post-review.** Never tag from a feature branch. If there's a parallel review peer still verifying merged-main, hold the tag until they ✅.

## Anti-patterns

- **Auto-tag on every merge.** Wastes version numbers on internal patches nobody outside the project cares about.
- **Tagging concurrent with merge.** Skips the post-merge review window where cross-model peer would catch regressions. PyPI publish is forward-only; pip-pinned consumers stay broken until they upgrade.
- **Bumping to 1.0 because 0.9.x feels close.** 1.0 is the user's intentional call. After 0.9.x comes 0.10.0.

## Tools to use

- `git log <last-tag>..main --oneline` — what's unreleased
- `gh pr list --state merged --base main --limit 10` — recent merges with PR refs
- `git tag --sort=-creatordate | head -5` — recent tags for context
