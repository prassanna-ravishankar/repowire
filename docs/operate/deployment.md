# Deployment

## Local deployment

Most users run only the local daemon installed by `repowire setup`. The dashboard is served from the daemon at `localhost:8377/dashboard`.

## Hosted surfaces

Repowire also ships hosted surfaces:

- Docs site: Zensical static site built by `.github/workflows/docs.yml`.
- Dashboard web bundle: built separately for the daemon and hosted web deployment.
- Relay: native Go relay deployed through the relay workflow and Helm chart.

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
