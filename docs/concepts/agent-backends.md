# Agent Backends

An agent backend is the runtime family Repowire knows how to install, spawn, resume, and identify.

## Current backends

| Backend | Runtime | Connection path |
| --- | --- | --- |
| `claude-code` | Claude Code 2.1.224+ | Hooks + MCP + authenticated native session inbox; optional experimental channel/ACP |
| `codex` | Codex | App Server threads + MCP |
| `opencode` | OpenCode | TypeScript plugin + WebSocket |
| `pi` | Pi | Native Repowire extension + WebSocket |
| `hosted` | Pi Durable on Cloudflare (prototype) | Daemon-owned; no local process |

Gemini CLI and Antigravity were retired as supported backends. Running `repowire setup` removes only Repowire-owned legacy entries from their configuration; existing persisted peer rows remain readable as historical data but cannot be spawned or resumed.

## Hosted agents

A hosted agent is a managed peer whose runtime is a durable agent on the hosted service, spawned with `spawn_peer(backend="hosted")`. It is a prototype.

- **Identity.** The daemon registers the peer itself, like the human peer. Its `peer_id` is also the hosted agent's id. Hosted peers cannot register through `POST /peers` or the WebSocket.
- **Sleeping is not offline.** A hosted peer holds no socket and stays `online` while its runtime sleeps. It is `busy` while a submission is in flight.
- **Delivery.** An ask, notify, or broadcast becomes a durable submission keyed by an operation id, framed as an untrusted `<peer-message>`. For an ask, the operation id is its correlation id, plus `-g<N>` once it has been delegated, so each assignment is its own run. Delivery succeeds only once the runtime accepts it; a refusal fails the send. Notifies and asks are recorded as [messages](message-types.md) with a `message-id`. A hosted agent always receives the full body, and the message reads as read once the runtime accepts it.
- **Replies.** The runtime's final answer closes the ask as `ack_with_msg`; a run that ends unanswered closes it `send_failed`. Notify and broadcast results are not sent anywhere. A report closes its ask only if the agent still holds it, under the same assignment. One that arrives during a hand-off is resent later; one from an agent the ask was delegated away from is dropped.
- **Mesh tools.** A hosted agent can call `list_peers`, `whoami`, `notify_peer`, `ask_peer`, and `set_description`. These are the names the agent's model sees; `ask_peer` runs the daemon's `ask` tool, and the others run the daemon tools of the same name. The daemon holds one WebSocket to the runtime's gateway, and each call runs as the hosted peer through the daemon's MCP tools. A reply to the agent's own ask arrives later as a new message (`[ack #id from @name]`). While the daemon is disconnected, these tools return an error; nothing is queued. Lifecycle tools (`spawn_peer`, `kill_peer`) and `ack`/`decline` are not offered.
- **Retiring.** `kill_peer` stops a hosted peer through its container plug: the runtime agent is retired first, then the `peer_id` is closed, as for every backend. The agent stops taking input (new submissions return 410) and keeps its transcript. If the runtime cannot be reached, the kill fails and the peer stays registered. A per-fire job executor is retired the same way when its attempt ends.
- **Retention.** The owner's Gateway on the hosted service keeps runs, relay requests and cross-machine records, and sweeps them by the daemon's [retention](../operate/state-and-storage.md#retention) policy. Each daemon names its policy on every gateway connect. With several machines, the Gateway keeps each class as long as any of them asks; without any policy it uses the same defaults. Usage rows are the billing ledger and are kept. Each agent sweeps its own per-operation records by the same policy. Once past the content age, a retired agent is deleted whole, conversation included, and so is a workspace no live agent uses. Its last snapshot expires on the platform within 30 days. A live agent's old transcript is kept, because the runtime has no API to expire it yet.
- **Jobs.** `job_create(backend="hosted")` runs durable jobs on hosted agents; see [jobs](../use/features/jobs.md). A hosted run's prompt and answer are recorded as chat turns when the run settles.
- **Restarts.** Each submission is a `hosted_delivery` operation until the runtime reports it settled. The runtime reports through the gateway and keeps every report until the daemon acknowledges it, so a run that finishes while the daemon is down is delivered when it reconnects. The daemon settles a report in steps (the reply, the job, the chat feed), records each step as it succeeds, and acknowledges the report only after all of them. A step that fails, or a daemon that stops mid-settlement, leaves the report with the runtime, and the resent report resumes at the first unfinished step. A step cut off partway runs again, so a reply or a job's owner notice can arrive twice in that case; a closed ask or finished job ignores the repeat. Calls to the runtime time out after 30 seconds, and one settlement after two minutes. A restarted daemon re-registers hosted peers from their durable mappings, keeps peers with unsettled work `busy`, and submits each unsettled delivery again under its operation id, since the previous daemon may have stopped before the runtime had it; the runtime ignores one it already holds. A resubmission that fails is logged and changes nothing: the delivery stays open and its peer `busy` until the runtime's genuine report settles it. A reply for an ask the restarted daemon no longer tracks reaches the asker as a notify, or its durable delivery queue if the asker has not reconnected.

The hosted runtime lives in `hosted/` in the repository. Configure it under [`hosted`](../reference/configuration.md#hosted).

## Why it exists

Backends let Repowire keep display names, peer ids, hook installers, spawn commands, profile args, and resume behavior separate from the human-facing project name. This matters when several same-path or same-name peers are alive at once.

## Related

- [Connect agent runtimes](../use/features/index.md#connect-agent-runtimes)
- [Configuration](../reference/configuration.md#daemonspawn)
- [Contributing: adding a backend](../contributing/adding-a-backend.md)
