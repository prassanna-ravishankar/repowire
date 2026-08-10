# Gemini CLI

Google's Gemini CLI. Hooks and MCP entries land in `~/.gemini/settings.json`.

## What gets installed

Both hooks and the MCP server live in the same `settings.json`. Repowire appends to existing arrays without clobbering user-defined entries.

### Hooks

Gemini CLI uses different event names than Claude Code or Codex:

| Gemini event | Mapped to | What it does |
| --- | --- | --- |
| `SessionStart` | session hook | Registers the peer with the daemon |
| `BeforeAgent` | prompt hook (`UserPromptSubmit` equivalent) | Marks peer `busy` |
| `AfterAgent` | stop hook (`Stop` equivalent) | Captures response, fetches pending asks, marks peer `online` |

The native normalizer in `daemon-go/hooks/common.go` maps these to canonical
names so the handler code does not branch on runtime.

### Hook output

Gemini hooks require an explicit decision in their JSON output. Repowire emits `{"decision": "allow"}` from `AfterAgent` so the agent continues normally. Claude Code and Codex Stop hooks emit a decision only when blocking to resurface an open ask.

### MCP server

The Repowire MCP identity shim is added under `mcpServers.repowire` in the same
`settings.json`, runs as `repowire mcp` over stdio, sets
`REPOWIRE_BACKEND=gemini`, and proxies tools to the daemon's `/mcp` endpoint.

## Response field

Where Claude Code dumps a transcript JSONL that the stop hook parses, and Codex emits `last_assistant_message`, Gemini emits `prompt_response`. The adapter pulls the right field per runtime — you don't need to think about this unless you're patching hooks.

## Verifying

```bash
repowire status
```

Open a Gemini session, send one message, watch `repowire peer list`. Gemini fires hooks promptly on first prompt.

## Troubleshooting

- Hook errors visible in Gemini output → confirm `repowire` is on `PATH` for the shell Gemini was launched from. Gemini surfaces hook stderr; Claude Code and Codex tend to swallow it.
- Peer stuck `busy` → the `AfterAgent` hook didn't fire or didn't return `{"decision": "allow"}`. Lazy repair can reset stale `busy`/`working` state after `daemon.stale_busy_timeout_seconds`; see [Ghost peers and stuck busy state](../../troubleshooting/ghost-peers.md).
