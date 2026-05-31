---
name: repowire-install
description: Install or update repowire and its skill pack from inside an agent session — ensure the repowire CLI/hooks are set up, install the skills, and verify the mesh works. Use on a fresh machine or to refresh skills to the latest.
---

# Install / update repowire + skills

Bring an agent session up to full repowire capability. This is an
install/update helper run from *inside* an agent — it assumes you can run shell
commands. (It is not a magical pre-install bootstrap; the repowire package
provides the CLI/hooks.)

## 1. Ensure repowire is installed

```bash
repowire --version    # already installed?
```
If missing, install the CLI and set up hooks/MCP:
```bash
uv tool install repowire        # or: pipx install repowire
repowire setup                  # daemon, hooks, MCP, service (the supported path)
```
`repowire setup` is the source of truth for daemon/hook/MCP/service install —
this skill never replaces it.

## 2. Install / update the skill pack

The skills live in the repowire repo under `skills/`. Install the set into your
agent's skills directory with the skills CLI (works for any agent, not just
Claude):
```bash
npx skills add prassanna-ravishankar/repowire        # scans skills/, installs the pack
# or a single skill:
npx skills add https://github.com/prassanna-ravishankar/repowire/tree/main/skills/cross-agent-review
```
Claude Code users can instead add the plugin marketplace:
```text
/plugin marketplace add prassanna-ravishankar/repowire
/plugin install repowire@repowire
```

## 3. Set skill defaults (optional)

Skills resolve a backend as: explicit arg > config default > safe fallback. Set
defaults in `~/.repowire/config.yaml`:
```yaml
skills:
  default_reviewer_backend: codex
  default_planner_backend: gemini
  default_delegate_backend: codex
  default_circle: default
```
Read one back with `repowire config get skills.default_reviewer_backend`.

## 4. Verify

```bash
repowire peer whoami   # you're on the mesh
repowire peer list     # see other agents
repowire doctor        # health + drift checks
```
If `peer whoami` / `peer list` fail, run `repowire setup` and re-check — don't
create an alternate local mesh.
