# Deployment

## Local deployment

Most users run only the local daemon installed by `repowire setup`. The dashboard is served from the daemon at `localhost:8377/dashboard`.

## Hosted surfaces

Repowire also ships hosted surfaces:

- Docs site: Zensical static site built by `.github/workflows/docs.yml`.
- Dashboard web bundle: built separately for the daemon and hosted web deployment.
- Relay: native Go relay deployed through the relay workflow and Helm chart.

## Hosted agent prototype

The `hosted/` package is a separate Cloudflare Worker deployment in this repo:
a single-owner runtime for [hosted agents](../concepts/agent-backends.md#hosted-agents).
Each agent is a Pi Durable conversation in a SQLite-backed Durable Object.
Optional Linux workspaces have their own sandbox lifetimes, may be shared by
several agents, and are checkpointed after each command; agents without a
workspace do not start containers.

The daemon connects to it when both `hosted.url` and `hosted.token` are set (see
[configuration](../reference/configuration.md#hosted)). It creates, submits to,
and retires agents over HTTP, and holds one WebSocket to the Worker's `/gateway`
for the agents' mesh tool calls and their run reports. The Worker enforces
per-owner and per-agent budgets and run limits. SaaS login, billing, and
connector execution are follow-up work. See `hosted/README.md` in the
repository for deployment, endpoints, and limits.

## Contributor notes

Run docs builds before touching public docs:

```bash
uvx --from zensical==0.0.43 zensical build --strict
```

Use the pre-PR hygiene check before opening a PR:

```bash
scripts/pre-pr-hygiene.sh
```

## Related

- [Contributing](../contributing/index.md)
- [Pre-PR hygiene](../contributing/pre-pr-hygiene.md)
- [Operate: relay](relay.md)
