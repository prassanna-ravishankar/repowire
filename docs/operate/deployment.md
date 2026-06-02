# Deployment

## Local deployment

Most users run only the local daemon installed by `repowire setup`. The dashboard is served from the daemon at `localhost:8377/dashboard`.

## Hosted surfaces

Repowire also ships hosted surfaces:

- Docs site: Zensical static site built by `.github/workflows/docs.yml`.
- Dashboard web bundle: built separately for the daemon and hosted web deployment.
- Relay: FastAPI relay deployed through the relay workflow and Helm chart.

## Contributor notes

Run docs builds before touching public docs:

```bash
uv run --no-project zensical build --strict
```

Use the pre-PR hygiene check before opening a PR:

```bash
python3 scripts/pre_pr_hygiene.py
```

## Related

- [Contributing](../contributing/index.md)
- [Pre-PR hygiene](../contributing/pre-pr-hygiene.md)
- [Operate: relay](relay.md)
