# vs cloud coding services

Covers [Devin](https://devin.ai/), [Cursor Background Agents](https://cursor.com/product), and [Codex Cloud](https://developers.openai.com/codex/cloud) — the family of services that run autonomous coding agents on remote infrastructure.

## What they are

- **Devin** (Cognition). Autonomous engineer that plans, executes, debugs, and deploys. As of early 2026, plans start at $20/mo (Core) with metered "Agent Compute Units"; Team is $500/mo for 250 ACUs.
- **Cursor Background Agents.** Asynchronous agents that run in isolated Ubuntu VMs in the cloud, clone the repo, work on a branch, and push back. Tightly integrated with the Cursor IDE.
- **Codex Cloud** (OpenAI). Cloud-isolated containers running the Codex agent with Skills, Automations, and multi-agent parallel execution. Internet access disabled during task execution; the agent only sees the cloned repo and a setup script.

## Workflow boundary

These services own **a remote machine and the agent that runs on it**. You hand off a task; the service produces a PR or a result. The agent runs where the service operates it, not on your laptop.

Repowire owns **the routing between agents on your machines**. Agents run wherever you started them (your laptop, a remote dev box, a tmux session you control), and the daemon connects them.

## Architecture difference

- **Cloud services.** Cloud-managed compute, cloud-managed agent runtime, repo cloned per task, ephemeral environment, vendor controls the trust boundary.
- **Repowire.** Local-first daemon, agents are CLI processes you started, no remote compute by default. The hosted relay is optional and only tunnels traffic — it does not run the agent.

The big difference is *where the agent process lives*. Cloud services run it; repowire routes between processes you run.

## When to use repowire instead

- You want to keep agent state, codebases, and credentials on your own machines.
- You're paying by API tokens and don't want a per-task or per-ACU cloud premium on top.
- You need multi-agent coordination with full context retained between turns. Cloud services typically reset per task; repowire sessions persist as long as the agent processes do.
- You want to mix runtimes (Claude Code, Codex, Gemini, OpenCode) freely.

## When to use a cloud coding service instead

- You want a remote agent to finish a well-scoped task and push a PR while you do something else, without you keeping a session warm.
- You don't want to manage agent processes, tmux sessions, or hooks on your own machine.
- Compliance or hardware constraints prevent you from running agents locally at the scale you need.
- The task is one-shot enough that the cloud service's "task in, PR out" shape fits.

## Can you use both?

Yes. Run Devin or Cursor BG for fire-and-forget tickets, and use repowire on your local machine for the work you're actively driving. A repowire orchestrator can even dispatch tickets to a cloud service through the same mesh primitives — `notify_peer` an infra-style peer that pokes the cloud service's API, then surfaces the result back.

The line is *who runs the agent*. Cloud services run it for you. Repowire connects agents you run.
