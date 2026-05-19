# Setup per agent

`repowire setup` auto-detects every installed agent runtime and wires the appropriate Repowire transport. These pages are for when the auto-detection breaks, or when you want to know what was wired and why.

- [Claude Code](claude-code.md) — hooks, MCP server, optional channel transport.
- [Codex](codex.md) — `~/.codex/hooks.json` + `config.toml`, late SessionStart timing.
- [Gemini CLI](gemini.md) — `BeforeAgent` / `AfterAgent` mapped to prompt/stop hooks.
- [OpenCode](opencode.md) — TypeScript plugin with persistent WebSocket.
- Pi — extension path installed when the `pi` CLI or config is detected.
