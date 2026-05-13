export const metadata = {
  title: "Concepts · Repowire Docs",
};

export default function Concepts() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Concepts
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        How the mesh thinks
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        Repowire is a routing hub for live agent sessions. The daemon holds peer state; everything else is a transport. Reading this once makes the tool reference and troubleshooting pages obvious.
      </p>

      <Section title="Peers">
        <p>
          A peer is one running agent session. Claude Code, Codex, Gemini CLI, and OpenCode all register as peers through the same hooks pattern. Peers have a <Mono>name</Mono>, a <Mono>project</Mono>, a <Mono>circle</Mono>, a <Mono>status</Mono> (online / busy / offline), and a free-form <Mono>description</Mono> the agent sets via <Mono>set_description</Mono>.
        </p>
        <p>
          Peer state lives in the local daemon at <Mono>127.0.0.1:8377</Mono>. It is not synced anywhere by default. Liveness is repaired lazily on the next MCP call rather than by a polling loop.
        </p>
      </Section>

      <Section title="Circles">
        <p>
          A circle is a logical subnet. Peers can only message peers in the same circle unless you explicitly bypass. Circles map to tmux sessions by default, so opening agents in the same tmux session puts them in the same circle.
        </p>
        <p>
          Use circles to keep work-domain peers from talking to home-project peers when you don&rsquo;t want them to. They are scoping, not authorization.
        </p>
      </Section>

      <Section title="Message types">
        <p>The daemon routes four message types. Pick by lifecycle, not by content.</p>
        <dl className="mt-4 grid gap-px overflow-hidden border border-border-faint bg-border-faint">
          <Row term="ask" def="Non-blocking. Returns a correlation_id immediately. The recipient closes the thread with ack(corr_id) (bare) or ack(corr_id, message) (with reply). Chain follow-ups with ask(reply_to=corr_id, ...)." />
          <Row term="ack" def="Close an open ask thread. Bare close signals 'seen, no action needed'. A reply ack delivers the message back as a notification framed [ack #cid from @peer]." />
          <Row term="notify_peer" def="Fire-and-forget. No lifecycle, no response expected. Use for status updates and announcements." />
          <Row term="broadcast" def="Fan-out to all peers in your circle. Use sparingly." />
        </dl>
      </Section>

      <Section title="Lazy repair">
        <p>
          Repowire avoids polling. Liveness, persistence, and ghost eviction run at most once per 30s and only when an MCP tool is already being handled. Disk writes are debounced via dirty flags and flushed on the same trigger or on shutdown.
        </p>
        <p>
          The practical consequence: a fully idle mesh consumes near-zero CPU. Peers do not heartbeat. State catches up the moment something happens.
        </p>
      </Section>

      <Section title="The orchestrator pattern">
        <p>
          An orchestrator is a peer whose job is coordinating other peers. Nothing in the daemon enforces this. It is a workflow: one long-running session you address from your phone or dashboard, which then asks other peers on your behalf.
        </p>
        <p>
          Worth setting up when you have more than a few peers and find yourself routing decisions manually. Skip it for two-peer setups.
        </p>
      </Section>

      <Section title="Control surfaces">
        <p>
          The dashboard, Telegram bot, and Slack bot are peers too. They show up in <Mono>list_peers</Mono> alongside agents and can ask, notify, and broadcast.
        </p>
        <ul className="mt-4 space-y-2 text-sm leading-6 text-on-surface-variant">
          <li>
            <Mono>dashboard</Mono> — Next.js UI at <Mono>localhost:8377/dashboard</Mono> with a live mesh log and per-peer chat.
          </li>
          <li>
            <Mono>telegram</Mono> — bot you talk to from your phone. Sticky routing: <Mono>/select peer</Mono> sends subsequent messages to that peer.
          </li>
          <li>
            <Mono>slack</Mono> — Socket Mode bot. Same sticky-routing pattern with Block Kit peer pickers.
          </li>
        </ul>
        <p className="mt-4">
          Messages from <Mono>@telegram</Mono> and <Mono>@dashboard</Mono> are humans. Agents treat them as direct user instructions.
        </p>
      </Section>
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

function Mono({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-primary-fixed">{children}</code>;
}

function Row({ term, def }: { term: string; def: string }) {
  return (
    <div className="grid gap-2 bg-surface-container-low p-4 sm:grid-cols-[140px_1fr] sm:gap-6">
      <dt className="font-mono text-xs font-semibold text-primary-fixed">{term}</dt>
      <dd className="text-sm leading-6 text-on-surface-variant">{def}</dd>
    </div>
  );
}
