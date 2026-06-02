# Agent Backends

An agent backend is the runtime family Repowire knows how to install, spawn, resume, and identify.

## Current backends

| Backend | Runtime | Connection path |
| --- | --- | --- |
| `claude-code` | Claude Code | Hooks + MCP; optional experimental channel/ACP |
| `codex` | Codex | Hooks + MCP |
| `gemini` | Gemini CLI | Hooks + MCP through normalized `BeforeAgent` / `AfterAgent` events |
| `antigravity` | Antigravity CLI (`agy`) | Plugin install verified; hook firing and MCP pending upstream verification |
| `opencode` | OpenCode | TypeScript plugin + WebSocket |
| `pi` | Pi | Repowire extension path |

Antigravity is not at parity with the hook-backed runtimes yet. Treat it as a CLI-fallback and plugin-integration lane until hook firing and MCP availability are verified upstream.

## Why it exists

Backends let Repowire keep display names, peer ids, hook installers, spawn commands, profile args, and resume behavior separate from the human-facing project name. This matters when several same-path or same-name peers are alive at once.

## Related

- [Connect agent runtimes](../use/features/index.md#connect-agent-runtimes)
- [Configuration](../reference/configuration.md#daemonspawn)
- [Contributing: adding a backend](../contributing/adding-a-backend.md)
