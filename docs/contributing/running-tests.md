# Running Tests

## Quality gates

```bash
cd daemon-go
gofmt -w .
go vet ./...
go test -race ./...
cd ../web
npm test -- --run
npm run build
```

CI runs the same core gates.

## Notes

- Route and WebSocket tests use `httptest` plus the native WebSocket client.
- Keep environment-sensitive runtime detection tests hermetic.
- Rebuild and re-run setup after hook changes because hooks run from the recorded binary.

## Related

- [Development setup](development-setup.md)
- [Pre-PR hygiene](pre-pr-hygiene.md)
