# Relay key rotation

The hosted relay at `repowire.io` authenticates your daemon via an API key stored in `~/.repowire/config.yaml` under `relay.api_key`. The key is auto-generated the first time you run `repowire serve --relay` (or `repowire setup --relay`).

## Rotating the key

There is no one-shot rotation command in the MVP. Manual steps:

1. Stop the daemon.
2. Edit `~/.repowire/config.yaml` and remove the `relay.api_key` line (keep `relay.enabled: true` and `relay.url`).
3. Restart the daemon (`repowire serve --relay` or the installed service).
4. The daemon registers fresh against the relay and writes a new `relay.api_key` back to the config file.

The dashboard cookie tied to the old key is invalidated. Anyone with a stale dashboard session will need to re-enter the key.

## Key compromised

If you have reason to believe the key was exposed (committed to a public repo, pasted in a screenshot), rotate immediately. Until you do, anyone with the key can reach your local daemon over the relay tunnel.

The relay only ever sees the encrypted tunnel; agent traffic between daemons rides through it. But the dashboard tunnel exposes your local HTTP API to anyone authenticated with the key.

## Self-hosted relay

If you're running your own relay (`repowire relay start`), the same flow applies, just against your relay's config. The hosted relay's behavior is not special — both flows write the same `relay.api_key` field.

## See also

- [Relay access](../use/features/relay-access.md) — what the relay does and how the tunnel works.
- [Security posture](../operate/security.md) — what the relay can and can't see.
