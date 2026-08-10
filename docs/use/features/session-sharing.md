# Session sharing

## What it is

Session sharing generates a shareable URL for a running agent peer. Anyone with
the link can watch the agent's live event stream in a browser — without needing
a relay key, a running daemon, or any Repowire installation. Optionally, the
link can allow the viewer to inject asks (read-write).

Links are scoped to a single peer. The rest of the mesh is not visible.

## When to use it

- Hand a link to a colleague so they can observe an agent mid-task without SSH
  access.
- Give a stakeholder a live view of a long-running job.
- Share a debugging session across machines.
- Let a reviewer inject instructions into an active agent (read-write).

Skip session sharing when the viewer needs full dashboard access. Use
[relay access](relay-access.md) for that.

## Requirements

The relay must be configured before sharing works:

```bash
repowire setup --relay
```

The daemon connects to the hosted relay at `repowire.io`. The share link is
served from the same relay.

## Quickstart

```bash
repowire share my-agent
```

Output:

```
Share link created — [ro]

  https://repowire.io/s/sh_xxxxxxxxxxxxxxxx

  share_id: sh_xxxxxxxxxxxxxxxx
  expires:  never

  Revoke:   repowire share --revoke sh_xxxxxxxxxxxxxxxx
```

Open the URL in any browser. No login required.

## Read-write links

```bash
repowire share my-agent --rw
```

The viewer page gains a compose box. Messages sent there arrive as asks from a
`guest` peer. The agent sees them the same as any other ask.

Use read-write links with people you trust — they can steer the agent.

## Expiry

Links do not expire by default. Set a TTL in seconds:

```bash
repowire share my-agent --ttl 3600   # expires in 1 hour
```

Once the link expires, the viewer page shows an expiry notice. Active SSE
connections also receive a `share_expired` event and close.

Links are invalidated when the relay process restarts. This is intentional:
restart = clean slate.

## List and revoke

```bash
repowire share --list

repowire share --revoke sh_xxxxxxxxxxxxxxxx
```

## From an agent (MCP)

Agents can generate share links for themselves. Call only when the user
explicitly asks — do not share proactively:

```text
share_session()                            # share yourself, read-only
share_session(permissions="rw")            # read-write
share_session(peer_name="other-agent")     # share a different peer
share_session(ttl_secs=1800)               # expires in 30 minutes
```

Revoke a link:

```text
revoke_share(share_id="sh_xxx")
```

## What the viewer sees

The viewer page is a minimal dark-themed page that:

- Shows the peer name and permissions badge.
- Streams live events (asks, chat turns, status changes) as they arrive.
- Filters events to the shared peer only — other mesh activity is not visible.
- (Read-write only) Has a compose area to send asks.

## Security model

- **Anyone with the link can view** (ro) or interact (rw). Treat share links as
  secrets — do not post them publicly.
- Read-only links cannot inject asks even if the viewer manipulates the request.
  The relay enforces permissions server-side.
- Links are not tied to the viewer's identity. There is no login step.
- All event strings (`type`, `from_peer`, `text`) are HTML-escaped before
  rendering — no script injection through event content.
- Revoking a link closes active SSE connections within the next keepalive cycle
  (≤ 15 seconds).

## See also

- [Relay access](relay-access.md) — full dashboard access for the mesh owner.
- [`repowire share`](../../reference/cli.md#repowire-share) — CLI reference.
- [`share_session`](../../reference/mcp-tools.md#share_session) — MCP tool reference.
