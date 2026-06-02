# Development Setup

Install Repowire in editable development mode and sync test dependencies:

```bash
uv sync --extra dev
uv tool install . --force-reinstall
```

Hooks run from the installed package, so reinstall after hook or installer changes.

## Useful commands

```bash
pytest
ruff check repowire/
uv run ty check repowire/
uv run --no-project zensical build --strict
```

## Related

- [Running tests](running-tests.md)
- [Documentation standards](documentation-standards.md)
