---
name: create-skill
description: Use when adding, updating, splitting, or reviewing local orchestrator skills for reusable procedures, including controlled self-improvement of the orchestrator.
---

# Create Skill

Use a skill when a procedure is reusable, conditional, and too detailed for always-loaded `AGENTS.md`.

Do not add a skill for one-off status, a current project decision, a private user workflow, or a rule that belongs in `comms.md`, `projects.md`, memory, or a project-local agent file.

## Shape

Create `./.agents/skills/<name>/SKILL.md` with required frontmatter:

```markdown
---
name: <name>
description: Use when ...
---

# <Title>
```

The description is the trigger surface. Make it concrete enough that an agent can load the skill only when relevant.

## Self-improvement loop

1. Identify the repeated failure or repeated procedure.
2. Decide whether the fix belongs in a skill, memory, board/process, docs, or code.
3. Add or update the smallest skill text that would change future behavior.
4. Keep examples generic unless the skill is explicitly local-only.
5. Validate install/update behavior if the skill is part of a shipped template.

## General versus local

Ship general mechanics: delegation, review cycles, durable jobs, handover, worktree isolation, cleanup.

Keep local-only: project-specific routing, named personal channels, account assumptions, private credentials, customer-specific policy, or personal release authority.

If a skill starts mixing both, split it: product-general guidance in the shipped skill, user-specific preferences in local memory or local-only skills.
