# Channel-mode auth failures

The experimental [channel transport](../use/features/connect-claude-code.md#channel-transport-experimental) routes messages directly through MCP instead of the default native-inbox bridge. It only works on Claude Code, and only with specific prerequisites.

## Hard requirements

- **Claude Code v2.1.80 or newer.** Run `claude --version` to check.
- **claude.ai login.** Channel transport does not work with API key or Console key auth. Sign in with `claude /login` against your claude.ai account.
- **bun on `PATH`.** The channel server is a Bun-runtime TypeScript program. Install from [bun.sh](https://bun.sh).

If any of these are missing, `repowire setup --experimental-channels` refuses to install and reports which one. Re-run after fixing.

## "Failed to authenticate"

The channel server starts but immediately exits with an auth error.

- Run `claude /status` and confirm you're signed in via claude.ai, not via API key. If it shows an API key, sign out and back in with `claude /login`.
- Tokens can rotate. Re-running `claude /login` refreshes them; the channel server picks up the new token on next start.
- If the daemon is configured with `daemon.auth_token`, re-run `repowire setup --experimental-channels` so the `repowire-channel` entry includes `REPOWIRE_AUTH_TOKEN`.

## Messages not arriving

The channel server is running but messages never reach Claude:

- Confirm the `repowire-channel` MCP server entry in `~/.claude.json` points at the channel server. The normal `repowire` MCP server should still be present separately for tools such as `ack`, `ask`, and `notify_peer`.
- Restart the Claude Code session. The channel server connects once at session start; if it disconnected, a restart re-establishes the WebSocket.

## Switching back to default transport

```bash
repowire setup
```

Re-running without `--experimental-channels` restores the default hooks transport. The channel server is left in place but unused; remove it explicitly with `repowire uninstall` if you want a clean slate.

## See also

- [Claude Code setup](../use/features/connect-claude-code.md) — what gets installed in channel vs default mode.
- [Daemon unreachable](daemon.md) — if the daemon is down, the channel server has nothing to talk to.
