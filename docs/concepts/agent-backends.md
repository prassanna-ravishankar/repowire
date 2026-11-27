# Agent Backends

An agent backend is the runtime family Repowire knows how to install, spawn, resume, and identify.

## Current backends

| Backend | Runtime | Connection path |
| --- | --- | --- |
| `claude-code` | Claude Code 2.1.224+ | Hooks + MCP + authenticated native session inbox; optional experimental channel/ACP |
| `codex` | Codex | App Server threads + MCP |
| `opencode` | OpenCode | TypeScript plugin + WebSocket |
| `pi` | Pi | Native Repowire extension + WebSocket |

Gemini CLI and Antigravity were retired as supported backends. Running `repowire setup` removes only Repowire-owned legacy entries from their configuration; existing persisted peer rows remain readable as historical data but cannot be spawned or resumed.

## Why it exists

Backends let Repowire keep display names, peer ids, hook installers, spawn commands, profile args, and resume behavior separate from the human-facing project name. This matters when several same-path or same-name peers are alive at once.

## Related

- [Connect agent runtimes](../use/features/index.md#connect-agent-runtimes)
- [Configuration](../reference/configuration.md#daemonspawn)
- [Contributing: adding a backend](../contributing/adding-a-backend.md)
