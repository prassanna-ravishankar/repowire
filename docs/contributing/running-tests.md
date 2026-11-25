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
- A test that needs tmux gets its own server from `internal/tmuxtest`, never by calling `tmux` itself. Tests run inside tmux panes, and a bare `tmux` (including a cleanup's `kill-server`) reaches the server named in `$TMUX`: the live one running your agents. `tmuxtest.New` names a private socket with `-S`, puts a shim first on `PATH` so the code under test reaches only that socket, refuses a socket that is the live server's, and stops the server only while its socket exists. `tmuxtest.FollowEnv()` is for code whose subject is how tmux reads `$TMUX` (hooks): the environment then points at the sandbox instead of a shim. On a host that cannot run a tmux server (an agent sandbox that denies the socket or a pane's terminal), these tests skip with tmux's diagnostic instead of failing.

## Related

- [Development setup](development-setup.md)
- [Pre-PR hygiene](pre-pr-hygiene.md)
