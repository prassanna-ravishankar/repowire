# Daemon

## What it runs

The daemon is the local FastAPI routing hub on `127.0.0.1:8377` by default. It owns peer registration, message routing, ask state, schedules, durable jobs, dashboard routes, attachments, relay client connections, and lazy repair.

## Service lifecycle

Run it in the foreground while debugging:

```bash
repowire serve
```

For normal use, `repowire setup` installs the service appropriate for the platform. Use `repowire status` and `repowire doctor` to inspect the local install.

## Operating notes

- The daemon is local-first. The relay is optional and uses outbound WSS.
- Auth is configured through `daemon.auth_token`.
- Repair work is request-driven; Repowire does not add polling loops for liveness or persistence.

## Related

- [Troubleshooting: daemon unreachable](../troubleshooting/daemon.md)
- [Operations: architecture](architecture.md)
- [Configuration](../reference/configuration.md)
