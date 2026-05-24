export const metadata = {
  title: "Python client · Repowire Docs",
};

export default function ClientReference() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Reference
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        Typed Python client
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        <Mono>repowire.client.AsyncRepowireClient</Mono> is the supported way to talk to the daemon from Python code that is not an agent. It wraps the daemon&rsquo;s HTTP API in a typed async surface using pydantic models for every response. Apps and scripts should depend on this rather than reach into daemon internals.
      </p>

      <Section title="Construction">
        <p>
          Pass a base URL and optional auth token. The default targets a local daemon at <Mono>127.0.0.1:8377</Mono>. The client is async-context-managed; <Mono>aclose()</Mono> is wired into <Mono>__aexit__</Mono>.
        </p>
        <Code>
{`from repowire.client import AsyncRepowireClient

async with AsyncRepowireClient() as client:
    health = await client.health()
    print(health.version)
    print(health.channel.status, health.acp_broker.status)

# with auth and a custom base
async with AsyncRepowireClient(
    "https://repowire.io",
    auth_token="rw_...",
    timeout=10.0,
) as client:
    peers = await client.list_peers(status="online")`}
        </Code>
        <p>
          You can also inject your own <Mono>httpx.AsyncClient</Mono> via the <Mono>client=</Mono> kwarg, in which case ownership stays with you and <Mono>aclose()</Mono> becomes a no-op.
        </p>
        <p>
          <Mono>health.channel</Mono> and <Mono>health.acp_broker</Mono> are passive readiness snapshots for channel runtime/auth state, ACP broker prompts, permission relay state, and the daemon&rsquo;s last recorded broker error.
        </p>
      </Section>

      <Section title="Identity">
        <p>
          Every routing call takes a <Mono>from_peer</Mono> kwarg. Unlike MCP tools, the client does not auto-detect identity from the tmux pane. Pass the registered name explicitly. Register first if the caller is not already a peer:
        </p>
        <Code>
{`reg = await client.register_peer(
    name="my-script",
    path="/home/me/scripts",
    backend="python",
)
print(reg.peer_id, reg.display_name)`}
        </Code>
      </Section>

      <Section title="Ask, ack, notify, broadcast">
        <p>
          The four routing primitives mirror the MCP tool surface. Asks are non-blocking and return a <Mono>correlation_id</Mono>; the recipient closes them with <Mono>ack</Mono>. Reply content rides on the same ack call.
        </p>
        <Code>
{`result = await client.ask(
    to_peer="project-b",
    text="Which port is the daemon on?",
    from_peer="my-script",
)
cid = result.correlation_id

# elsewhere, on the recipient side or from an orchestrator:
await client.ack(cid, from_peer="project-b", message="8377")

await client.notify(
    to_peer="telegram",
    text="long task done",
    from_peer="my-script",
)

bc = await client.broadcast(
    "rebasing main",
    from_peer="my-script",
)
print(bc)`}
        </Code>
      </Section>

      <Section title="Listing and inspection">
        <p>
          Pull current mesh state. <Mono>list_peers</Mono> accepts daemon-supported filters; <Mono>get_peer</Mono> resolves a single peer by name or id. <Mono>pending_asks</Mono> returns open asks for one pane or peer; pass <Mono>direction=&quot;outbound&quot;</Mono> or <Mono>direction=&quot;both&quot;</Mono> to inspect asks opened by that peer.
        </p>
        <Code>
{`for peer in await client.list_peers(status="online"):
    print(peer.name, peer.circle, peer.description)

peer = await client.get_peer("project-b")

asks = await client.pending_asks(peer_id=peer.peer_id)
outbound = await client.pending_asks(peer_id=peer.peer_id, direction="outbound")`}
        </Code>
      </Section>

      <Section title="Spawning and lifecycle">
        <p>
          <Mono>spawn</Mono> launches a new agent session subject to <Mono>daemon.spawn.commands</Mono> and <Mono>daemon.spawn.allowed_paths</Mono>. <Mono>spawn_config</Mono> reports which backend launch profiles are configured. Omit <Mono>circle</Mono> to use the daemon default (<Mono>default</Mono>), or pass it explicitly for another circle. <Mono>kill_peer</Mono> terminates a peer cleanly.
        </p>
        <p>
          <Mono>restart_peer</Mono> intentionally restarts a daemon-spawned peer on the same backend, path, circle, role, and mesh identity. It refuses cross-host peers and panes the daemon cannot prove it spawned with explicit ownership proof plus live tmux evidence. Manually attached peers, stale pane records, and mismatched live pane evidence are refused instead of killed. Restart is same-window/name first, not same-pane; tmux allocates a fresh pane through the normal spawn path. The response includes <Mono>resume_mode</Mono>; <Mono>fresh_runtime_context</Mono> reloads startup context but does not guarantee transcript replay or exact backend conversation resume.
        </p>
        <Code>
{`info = await client.spawn_config()
if "claude-code" in info.commands:
    spawn = await client.spawn(
        path="/home/me/projects/project-c",
        backend="claude-code",
        circle="docs",
        message="help me draft a reference page",
    )
    print(spawn.display_name, spawn.tmux_session)

restart = await client.restart_peer("project-c-claude-code", dry_run=True)
print(restart.status, restart.resume_mode)`}
        </Code>
      </Section>

      <Section title="Errors">
        <p>
          All methods raise one of three typed errors from <Mono>repowire.protocol.errors</Mono>:
        </p>
        <ul className="mt-2 space-y-2">
          <li>
            <Mono>DaemonConnectionError</Mono>: the daemon is not reachable (most often, not running).
          </li>
          <li>
            <Mono>DaemonTimeoutError</Mono>: the daemon accepted the connection but did not respond in time.
          </li>
          <li>
            <Mono>DaemonHTTPError(status, body)</Mono>: the daemon returned a non-2xx response.
          </li>
        </ul>
      </Section>

      <Section title="Stability">
        <p>
          The client is the public Python surface; depend on it rather than the daemon HTTP routes, which may shift between releases. Repowire is pre-1.0, so method signatures and pydantic models may still adjust across minor versions. Additions are preferred over breaks, but explicit breaks will happen when the design wants them.
        </p>
      </Section>

      <div className="mt-12 border-t border-border-faint pt-8">
        <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
          See also
        </div>
        <p className="text-sm leading-6 text-on-surface-variant">
          Agents call the same primitives through <a className="text-primary-fixed underline-offset-4 hover:underline" href="/docs/reference/tools">MCP tools</a>. The semantics are identical; only the identity resolution differs.
        </p>
      </div>
    </article>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="mt-10">
      <h2 className="font-headline text-xl font-semibold text-on-surface">{title}</h2>
      <div className="mt-4 space-y-4 text-sm leading-6 text-on-surface-variant">{children}</div>
    </section>
  );
}

function Code({ children }: { children: React.ReactNode }) {
  return (
    <pre className="overflow-x-auto border border-border-faint bg-surface-container-low p-4 font-mono text-xs leading-6 text-on-surface">
      <code>{children}</code>
    </pre>
  );
}

function Mono({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-primary-fixed">{children}</code>;
}
