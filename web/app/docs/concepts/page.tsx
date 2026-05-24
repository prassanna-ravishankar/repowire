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

      <Section title="Session-native roadmap">
        <p>
          The v0.13 architecture train is moving toward a session-first mesh: sessions become the durable unit of work, while peers remain live runtime executors. This is roadmap, not a claim that the product is fully session-native today.
        </p>
        <p>
          Ask/notify delivery now goes through a transport router. The broader direction is transport-neutral routing across WebSocket hooks, experimental ACP, relay, and future transports; a dashboard session timeline that combines persisted history with realtime events; and a shared command surface for send, resume, schedule, approvals, and future backend/model controls.
        </p>
      </Section>

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

      <Section title="Peer identity lifecycle">
        <p>
          The daemon routes by immutable <Mono>peer_id</Mono>, not by display name alone. Display names are human-facing and can collide across circles, so ambiguous name lookups refuse to guess unless you pass an explicit <Mono>circle</Mono> or use a <Mono>peer_id</Mono>.
        </p>
        <p>
          Reconnects may reclaim a peer id only when the claim still matches the registered backend and path. Stale task descriptions are bounded by a clear-on-read TTL, and routing events record resolved peer ids so misroutes can be diagnosed without ad hoc logs.
        </p>
      </Section>

      <Section title="Message types">
        <p>The daemon routes four message types. Pick by lifecycle, not by content.</p>
        <dl className="mt-4 grid gap-px overflow-hidden border border-border-faint bg-border-faint">
          <Row term="ask" def="Non-blocking. Returns a correlation_id immediately. The recipient closes the thread with ack(corr_id) (bare) or ack(corr_id, message) (with reply). Chain follow-ups with ask(reply_to=corr_id, ...)." />
          <Row term="ack" def="Close an open ask thread. Bare close signals 'seen, no action needed'. A reply ack delivers the message back as a notification framed [ack #cid from @peer]." />
          <Row term="notify_peer" def="Fire-and-forget. No lifecycle, no response expected. Use for status updates and announcements." />
          <Row term="broadcast" def="Fan-out to all peers in your circle. Use sparingly." />
          <Row term="schedule" def="Future delivery through the daemon. One-shot and recurring cron schedules can notify or open asks later." />
        </dl>
      </Section>

      <Section title="Mesh command UX">
        <p>
          Repowire&rsquo;s command layer has a stable contract for common mesh operations:
          <Mono>status</Mono>, <Mono>peers</Mono>, <Mono>pending-asks</Mono>, <Mono>ask</Mono>, <Mono>notify</Mono>, <Mono>schedule</Mono>, <Mono>timeline</Mono>, <Mono>result</Mono>, and <Mono>doctor</Mono>.
        </p>
        <p>
          Every command should have a human rendering for steering the mesh and a JSON rendering for agents, plugins, tests, and scripts. The JSON envelope carries <Mono>command</Mono>, <Mono>status</Mono>, <Mono>schema_version</Mono>, <Mono>data</Mono>, plus optional <Mono>target</Mono>, <Mono>warnings</Mono>, and <Mono>next_actions</Mono>.
        </p>
        <p>
          Agents use Repowire tools for mesh peers: <Mono>ask</Mono> for tracked work, <Mono>notify_peer</Mono> for fire-and-forget updates, and <Mono>ack</Mono> to close inbound asks. <Mono>SendMessage</Mono> is only for same-session harness teammates.
        </p>
        <p>
          <Mono>timeline</Mono> and <Mono>result</Mono> are views over existing peer, ask, schedule, event, and session-history data until a separate tracked-work lifecycle exists. ACP/channel broker health is reserved for the channel health work rather than claimed by this command contract.
        </p>
        <p>
          Future Claude Code marketplace plugin packaging may expose these commands as slash commands, skills, docs, and an MCP bootstrap, but it remains optional. The plugin manifest should map to the same command ids and check drift against the installed Repowire package, <Mono>repowire mcp</Mono>, hook snippets, Claude Code version, and the declared compatible Repowire range. It does not replace <Mono>repowire setup</Mono> or install a second daemon.
        </p>
      </Section>

      <Section title="Tracked work lifecycle">
        <p>
          Durable tracked work is a separate daemon-backed lifecycle from conversational <Mono>ask</Mono>/<Mono>ack</Mono>. The design reserves <Mono>work_id</Mono> records with states such as <Mono>queued</Mono>, <Mono>delivered</Mono>, <Mono>running</Mono>, <Mono>awaiting_input</Mono>, <Mono>completed</Mono>, <Mono>failed</Mono>, <Mono>cancelled</Mono>, <Mono>blocked</Mono>, <Mono>expired</Mono>, and <Mono>unavailable</Mono>.
        </p>
        <p>
          Status, result, and cancel semantics belong to that work lifecycle. Acks may close related conversation threads, but they do not complete work. Session and circle visibility should resolve by exact ids where possible, and protocol cancel should be attempted before transport teardown when a live backend connection can still accept it.
        </p>
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

      <Section title="Personas">
        <p>
          Orchestrator personas are local <Mono>SOUL.md</Mono> files that define identity, voice, and standing preferences. Repowire resolves workspace personas from <Mono>~/.repowire/orchestrator/personas/&lt;name&gt;/SOUL.md</Mono> before global personas in <Mono>~/.repowire/personas/&lt;name&gt;/SOUL.md</Mono>.
        </p>
        <p>
          <Mono>repowire orchestrator persona use &lt;name&gt;</Mono> marks the active persona. On SessionStart, orchestrator peers receive a persona context block with the resolved source and SHA-256 short hash. This is identity guidance, not a permission policy.
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
            <Mono>telegram</Mono> — bot you talk to from your phone. Sticky routing: <Mono>/select peer</Mono> sends subsequent messages as asks to that peer; <Mono>/notify</Mono> and <Mono>/fyi</Mono> remain fire-and-forget.
          </li>
          <li>
            <Mono>slack</Mono> — Socket Mode bot. Same sticky-routing pattern with Block Kit peer pickers; <Mono>notify</Mono> and <Mono>fyi</Mono> remain fire-and-forget.
          </li>
        </ul>
        <p className="mt-4">
          Messages from <Mono>@telegram</Mono>, <Mono>@slack</Mono>, and <Mono>@dashboard</Mono> are humans. Telegram and Slack inbound messages open tracked ask threads by default, and agents treat them as direct user instructions.
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
