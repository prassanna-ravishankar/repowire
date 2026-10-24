# Deployment

## Local deployment

Most users run only the local daemon installed by `repowire setup`. The dashboard is served from the daemon at `localhost:8377/dashboard`.

## Hosted surfaces

Repowire also ships hosted surfaces:

- Docs site: Zensical static site built by `.github/workflows/docs.yml`.
- Dashboard web bundle: built separately for the daemon and hosted web deployment.
- Relay: native Go relay deployed through the relay workflow and Helm chart.

## Hosted agent prototype

The `hosted/` package is a separate Cloudflare Worker deployment in this repo.
It provides an authenticated, single-owner HTTP prototype for spawning Pi Durable
agents, submitting messages with idempotency keys, and retrieving results.
Agent conversation state lives in SQLite-backed Durable Objects. Optional Linux
workspaces have separate sandbox lifetimes and may be shared by several agents.
A command checkpoints its workspace before the sandbox stops; agents without a
workspace do not start containers.

This is not connected to daemon/relay routing yet. SaaS login, billing, quotas,
Composio connector execution, and AI Gateway configuration are follow-up work.
See `hosted/README.md` in the repository for configuration, endpoints, and limits.

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
