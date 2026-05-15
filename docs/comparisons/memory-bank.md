# vs Memory Bank

## What it is

[Memory Bank](https://docs.cline.bot/features/memory-bank) is a community-developed methodology for [Cline](https://cline.bot/) (the VS Code agent) that gives the agent persistent context across sessions via structured markdown files. Files like `projectbrief.md`, `productContext.md`, `activeContext.md`, `systemPatterns.md`, `techContext.md`, and `progress.md` live in a `memory-bank/` folder. Custom instructions tell Cline to read them at the start of every session.

It is a *methodology*, not a built-in product feature — the Cline team explicitly describes it that way. Similar patterns exist for other agents (Claude Code's `MEMORY.md`, Cursor notepads, RooCode persistent context).

## Workflow boundary

Memory Bank's job is **giving a single agent persistent context across stateless restarts**. When a session ends, the markdown files remain; the next session reads them and reconstructs the project picture.

Repowire's job is **letting many live agents address each other while their sessions are running**. State is in the live process; repowire does not give one agent a memory of what it knew yesterday.

## Architecture difference

- **Memory Bank.** File-based, async, single-agent. The agent reads files; there is no other party in the loop.
- **Repowire.** Process-based, sync (`ask` is non-blocking but real-time), multi-agent. Routes messages between live sessions; persists nothing about agent *thinking*, only mesh state (peer registry, event log).

Different problems: persistent project context vs live cross-agent routing.

## When to use repowire instead

- Two agents need to talk *right now* about something that isn't in either's notes yet.
- You want the answer from the other repo's *current code*, not a paraphrase a previous session committed to markdown.
- You have a multi-agent workflow and need a control surface that aggregates them (dashboard, Telegram).

## When to use Memory Bank instead

- You're a solo agent on a long-lived project and the per-session context limit is the bottleneck.
- You want a paper trail of project decisions that survives the agent's context window.
- You don't have other live agents to ask, or your workflow is fundamentally single-agent.

## Can you use both?

Yes, and they compose well. A repowire peer can maintain its own Memory Bank for persistent project context, while still using repowire to route asks to other agents during live sessions. The two systems answer different questions:

- *"What did we decide about auth last week?"* → Memory Bank (your notes).
- *"What does the auth code currently look like?"* → repowire `ask` to the auth-owning peer.

Treat Memory Bank as the agent's notebook and repowire as the agent's phone.
