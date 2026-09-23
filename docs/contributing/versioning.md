# Versioning

Repowire is pre-1.0.

- **Patch** (`0.x.Y`): bug fixes, cleanup, small additions.
- **Minor** (`0.X.0`): significant new features or breaking changes.
- After `0.9.x` we move to `0.10.0`, `0.11.0`, and so on. `1.0.0` is an intentional decision, not an automatic bump.

The CLI, MCP, and HTTP surfaces prefer additions over breaks, but explicit breaks happen between minor versions when the design wants them.

## Releasing

Bump `Version` in `daemon-go/cli/commands.go`, commit `release: vX.Y.Z`, tag, and push the tag. The `Publish native release` workflow builds the archives, publishes the GitHub Release, and then bumps `Formula/repowire.rb` in the Homebrew tap (`prassanna-ravishankar/homebrew-repowire`) from the release's `checksums.txt`, pushing straight to the tap's `main`. That last job needs a `HOMEBREW_TAP_TOKEN` repository secret: a fine-grained personal access token with **Contents: read and write** on the tap repository only. Without it the release still publishes and the job fails loudly, leaving Homebrew users on the previous version.
