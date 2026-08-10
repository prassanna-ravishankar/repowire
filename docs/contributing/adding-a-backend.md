# Adding a backend

Repowire keeps the serialized backend identity in `daemon-go/proto` and the
runtime-specific launch/resume/setup behavior in the native service and CLI
packages.

## Required changes

1. Add the persisted `AgentType` value and validation case in
   `daemon-go/proto/proto.go`. Changing an existing value is a state migration.
2. Add resume capability and command construction in
   `daemon-go/service/agent_backends.go`.
3. Add CLI detection, setup/uninstall, MCP configuration, and embedded runtime
   assets in `daemon-go/cli/`.
4. Add native hook normalization in `daemon-go/hooks/` or an embedded
   WebSocket/plugin transport when the runtime has no hook surface.
5. Add the default spawn command in setup and document any runtime-specific
   session storage used for safe resume prevalidation.

Keep backend-specific decisions at these seams. Routes, jobs, peer state, and
delivery should continue operating on `proto.AgentType` without runtime-name
conditionals.

## Resume contract

Runtime resume is a backend capability, not a jobs-only special case. When a
hook reports a runtime session ID, Repowire records it and later acquires an
executor in this order:

1. reuse a suitable live peer;
2. prevalidate and spawn the backend-native resume command;
3. spawn fresh when no durable binding is usable.

The prevalidation step must prove the session exists before any pane is killed.

## Tests

At minimum, cover:

- `AgentType.Valid` and wire serialization;
- detection plus setup/uninstall behavior;
- resume capability, prevalidation, and command construction;
- spawn/session acquisition for reuse, resume, and fresh execution;
- MCP config edits when supported;
- hook/plugin registration and one real delivery round trip.
