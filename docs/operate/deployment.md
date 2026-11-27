# Deployment

## Local deployment

Most users run only the local daemon installed by `repowire setup`. The dashboard is served from the daemon at `localhost:8377/dashboard`.

## Hosted surfaces

Repowire also ships hosted surfaces:

- Docs site: Zensical static site built by `.github/workflows/docs.yml`.
- Dashboard web bundle: built separately for the daemon and hosted web deployment.
- Relay: native Go relay deployed through the relay workflow and Helm chart.

## Managed agents (prototype)

The `hosted/` package is a separate Cloudflare Worker deployment in this repo:
the runtime for managed agents. Each agent is a pi-durable conversation in a
SQLite-backed Durable Object. Optional Linux workspaces have their own sandbox
lifetimes, may be shared by several agents, and are saved to R2 when a turn
ends; agents without a workspace do not start containers.

Only the relay talks to it, with its control token: the relay serves `/managed`,
proxies the managed API, and treats the Worker as the `managed` machine, so
daemons and their agents reach managed agents through [cross-machine
circles](../concepts/peers-and-circles.md#circles-across-machines), and managed
agents run their mesh tools through the relay. See [relay](relay.md#managed-agents-control-plane)
for its configuration. The Worker enforces per-owner and per-agent budgets and
run limits. SaaS login, billing, and connector execution are follow-up work.
See `hosted/README.md` in the repository for deployment, endpoints, and limits.

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
