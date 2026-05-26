# Adding a backend

Repowire keeps backend identity and backend behavior separate:

- `repowire/agent_types.py` defines `AgentType`, the serialized enum used in
  config, APIs, peer state, jobs, and session bindings.
- `repowire/agent_backends.py` defines `AgentBackend`, the behavior registry
  for setup, spawn defaults, resume commands, MCP config, and post-spawn
  warmup.

Add a backend by updating both layers. Avoid scattering backend-specific
`if backend == ...` checks through routes, jobs, or CLI commands; put runtime
differences on the backend class and call through the registry.

## Backend class

Create an `AgentBackend` subclass in `repowire/agent_backends.py`:

```python
class ExampleBackend(AgentBackend):
    agent_type = AgentType.EXAMPLE
    display_name = "Example"
    cli_names = ("example",)
    default_command = "example"
    supports_resume = True
    resume_strategy = "example_resume"
    resume_flag = "--resume"
```

Register it in `AGENT_BACKENDS`. The registry must cover every `AgentType`, even
non-local backends that cannot be spawned. Use `supports_local_spawn = False`
and `default_command = None` for transport-only entries.

## Required decisions

| Decision | Where it lives | Notes |
| --- | --- | --- |
| Serialized name | `AgentType` | This is persisted state, so changing it is a migration. |
| Default spawn command | `AgentBackend.default_command` | Re-exported as `DEFAULT_SPAWN_COMMANDS` for config compatibility. |
| CLI detection | `cli_names` and `config_markers` | Used by `repowire setup` auto-detection. |
| Setup behavior | `install()` | Delegate heavy work to `repowire/installers/<backend>.py`. |
| Resume shape | `supports_resume`, `resume_flag`, `resume_subcommand` | Used by durable jobs and session acquisition to build backend-native resume commands. |
| MCP config support | `mcp_config_scope` and MCP methods | Only set this when Repowire can list/add/remove MCP servers for that backend. |
| Post-spawn warmup | `post_spawn_strategy` or `post_spawn_warmup()` | Use this for runtime-specific first-message or seed behavior. |

## Resume contract

Runtime resume is a backend capability, not a jobs-only special case.

When hooks or runtime registration expose a backend session ID, Repowire records
that ID in session/job provenance and asks the backend for `resume_capability`.
Later, session acquisition chooses in order:

1. reuse a suitable live peer;
2. spawn a backend-native resume command for the recorded runtime session;
3. spawn a fresh peer when no binding is usable.

For simple CLIs, set one of:

- `resume_flag = "--resume"` to run `<command> --resume <runtime-session-id>`;
- `resume_subcommand = "resume"` to run `<command> resume <runtime-session-id>`.

If the backend supports resume but needs more than a flag or subcommand,
override `build_resume_command()`.

## MCP config

Backends with MCP configuration support should define `mcp_config_scope` and
implement:

- `list_mcp_servers()`;
- `add_mcp_server()`;
- `remove_mcp_server()`.

Backends without MCP support should leave `mcp_config_scope = None`; the base
class returns a typed unsupported error.

## Tests

At minimum, add or update tests for:

- the registry invariant: `set(AGENT_BACKENDS) == set(AgentType)`;
- default command compatibility through `DEFAULT_SPAWN_COMMANDS`;
- setup auto-detection and installer dispatch;
- resume capability and resume command construction;
- spawn/session acquisition behavior for reuse, resume, and fresh spawn;
- MCP config behavior if the backend supports MCP edits;
- post-spawn warmup if the backend needs runtime-specific seeding.

Use focused tests for the backend class and one integration-level test for the
daemon or CLI path that consumes it.
