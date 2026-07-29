# First-run ritual

This file is the orchestrator's first-turn checklist. Run through it once, then delete this file (it's a one-time ritual, not persistent state).

## What to collect

Walk through these with the user. **Don't ask cold** — probe the environment first and present options. Asking "what are your projects?" is lazy when you can grep the filesystem.

### 0. Probe the mesh first

Before anything else, confirm you can actually do the work. A fresh orchestrator that dispatches into the void on turn one is a worse experience than one that admits "the daemon is down" up front.

```bash
# Is the daemon reachable?
curl -fsS http://127.0.0.1:8377/health || echo "(daemon not running — run 'repowire serve')"

# What peers are already online?
# Use the list_peers MCP tool, not curl — your registration may not be visible yet via HTTP cache.

# What runtimes does the user have? (matters when you later dispatch peers)
for r in pi claude codex gemini opencode; do command -v "$r" && echo "  $r: available"; done
```

If the daemon is down, surface that to the user immediately — don't continue the ritual until they fix it. If runtimes are missing, note them; you may need to suggest the user install them when they ask for work in those backends.

Once the mesh is healthy, claim your seat: call `set_description("orchestrator — fresh install, running BOOTSTRAP")` so peers see who's in the seat.

### 1. Active projects

Probe before asking:

```bash
# What's the user's GitHub identity?
gh auth status
gh api user -q .login

# What repos exist locally? Try common roots.
ls -d ~/git/* ~/development/projects/* ~/code/* ~/projects/* ~/work/* 2>/dev/null | head -50

# What does GitHub say they own?
gh repo list --limit 30 --json name,description
```

Present the discovered list: "I see these repos, which are active for orchestration?" Let them check a subset. Write the chosen ones into `projects.md` with one-line descriptions (pull from `gh repo view` if needed).

### 2. Primary comms channel

Ask: "Are you primarily on phone (Telegram) or laptop (dashboard) when reaching me?"

- **Phone (Telegram)** → set `comms.md` primary to telegram, push all updates via `notify_peer('telegram', ...)`.
- **Laptop (dashboard)** → set primary to dashboard.
- **Both** → default to dashboard, escalate to telegram when the message is urgent or short.

### 3. Two-model critique threshold

Ask: "For architectural-but-bounded changes (single PR, <500 LoC, but touches multiple layers), should I run a two-model critique by default?"

- **For high-risk architectural work** — spawn a different-model peer in the same worktree when an independent plan review materially reduces risk
- **PR-size-based** — only for PRs >300 lines or that touch >3 files
- **Never** — skip unless explicitly asked

Default: **always for architectural** (this is where two-model catches the most blind spots).

### 4. Explicit dislikes

Ask: "Anything I should explicitly avoid in how I communicate?" Pre-check these defaults from the lived practice:

- [x] No em-dashes
- [x] No marketing fluff or salesy openers
- [x] No score-keeping framing (don't report counts; lead with impact)
- [ ] _(add anything they raise)_

Write the chosen list into `comms.md` under "Voice and tone".

## When done

1. Verify `projects.md`, `comms.md`, and `orchestrator.yaml` reflect what you collected.
2. Confirm with the user: "I've set this up — does it look right?"
3. Delete this file: the ritual is one-time, not part of ongoing operating state.

```bash
rm BOOTSTRAP.md
```

## Things NOT to ask

- Name, role, bio — irrelevant to your work
- "What kind of orchestrator do you want me to be?" — pretentious; the user shouldn't have to author your job description
- Anything inferable from `gh auth status` / `git config` / filesystem probes
