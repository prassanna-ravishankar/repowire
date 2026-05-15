# Channel-mode auth failures

The experimental [channel transport](../agents/claude-code.md#channel-transport-experimental) routes messages directly through MCP instead of tmux injection. It only works on Claude Code, and only with specific prerequisites.

## Hard requirements

- **Claude Code v2.1.80 or newer.** Run `claude --version` to check.
- **claude.ai login.** Channel transport does not work with API key or Console key auth. Sign in with `claude /login` against your claude.ai account.
- **bun on `PATH`.** The channel server is a Bun-runtime TypeScript program. Install from [bun.sh](https://bun.sh).

If any of these are missing, `repowire setup --experimental-channels` refuses to install and reports which one. Re-run after fixing.

## "Failed to authenticate"

The channel server starts but immediately exits with an auth error.

- Run `claude /status` and confirm you're signed in via claude.ai, not via API key. If it shows an API key, sign out and back in with `claude /login`.
- Tokens can rotate. Re-running `claude /login` refreshes them; the channel server picks up the new token on next start.

## Messages not arriving

The channel server is running but messages never reach Claude:

- Confirm the MCP server entry in `~/.claude/settings.json` points at the channel server, not the regular stdio MCP server. `repowire setup --experimental-channels` writes the right entry; if you've hand-edited it, re-run setup.
- Restart the Claude Code session. The channel server connects once at session start; if it disconnected, a restart re-establishes the WebSocket.

## Switching back to default transport

```bash
repowire setup
```

Re-running without `--experimental-channels` restores the default tmux-injection transport. The channel server is left in place but unused; remove it explicitly with `repowire uninstall` if you want a clean slate.

## See also

- [Claude Code setup](../agents/claude-code.md) — what gets installed in channel vs default mode.
- [Daemon unreachable](daemon.md) — if the daemon is down, the channel server has nothing to talk to.
