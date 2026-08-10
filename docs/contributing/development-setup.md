# Development Setup

Build the dashboard and native binary from source:

```bash
cd web && npm ci && npm run build && cd ..
mkdir -p bin
(cd daemon-go && go build -o ../bin/repowire .)
./bin/repowire setup --non-interactive
```

Hooks run from the binary recorded by setup, so rebuild it after hook or installer changes.

## Useful commands

```bash
cd daemon-go && gofmt -w . && go vet ./... && go test -race ./...
cd web && npm test -- --run && npm run build
uvx --from zensical==0.0.43 zensical build --strict
```

## Related

- [Running tests](running-tests.md)
- [Documentation standards](documentation-standards.md)
