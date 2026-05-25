export const metadata = {
  title: "MCP tools · Repowire Docs",
};

export default function ToolsReference() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Reference
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        MCP tools
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        Every agent in the mesh exposes the same set of MCP tools through the repowire server. Tool calls go to the local daemon over HTTP; the agent never sees daemon internals. Names are stable and used identically across Claude Code, Codex, Gemini CLI, and OpenCode.
      </p>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        The stable transport is the stdio server installed by <Mono>repowire setup</Mono>. The experimental localhost Streamable HTTP endpoint can be enabled with <Mono>repowire setup --http-mcp</Mono>; clients connect to <Mono>http://127.0.0.1:8377/mcp</Mono> with <Mono>Authorization: Bearer &lt;daemon.auth_token&gt;</Mono>. HTTP MCP is local-only, is not relayed, and disables spawn, kill, and schedule mutation unless explicitly opted in with <Mono>daemon.mcp_http.allow_dangerous_tools</Mono>.
      </p>

      <Tool
        name="ask"
        signature={`ask(peer_name: str, query: str, reply_to: str | None = None, circle: str | None = None, attachments: list[dict] | None = None) -> str`}
      >
        <p>
          Open a non-blocking ask thread. In normal use, you tell your local agent what you need in natural language, and the agent invokes this MCP tool. Returns a <Mono>correlation_id</Mono> immediately. The recipient closes the thread with <Mono>ack</Mono>; the daemon routes the close back as a notification framed <Mono>[ack #cid from @peer]</Mono>.
        </p>
        <p>
          Daemon events for asks and acks include nullable <Mono>repowire_session_id</Mono>, <Mono>from_repowire_session_id</Mono>, and <Mono>to_repowire_session_id</Mono> fields when an existing session binding can be resolved. Peer IDs remain the routing authority.
        </p>
        <p>
          Live delivery is attempted first. If a CLI-fallback or polling peer has no live transport, the ask stays open and a one-shot SQLite queued delivery is stored for its next Stop-hook or CLI drain. The queued delivery is deleted after drain; the ask itself continues to appear in <Mono>/asks/pending</Mono> until <Mono>ack</Mono>.
        </p>
        <p>
          Pass <Mono>reply_to</Mono> to chain a follow-up: the prior thread closes and a new one opens referencing it. Pass <Mono>circle</Mono> only when two peers share a name in different circles.
        </p>
        <Example>
{`ask("project-b", "What API endpoints do you expose?")
# returns "ask-c1a1c7dd"`}
        </Example>
      </Tool>

      <Tool
        name="ack"
        signature={`ack(correlation_id: str, message: str | None = None, attachments: list[dict] | None = None) -> str`}
      >
        <p>
          Close an open ask. Bare <Mono>ack(cid)</Mono> signals &ldquo;seen, no action needed.&rdquo; A reply <Mono>ack(cid, message)</Mono> closes the thread and delivers the message back to the original asker. Replies always reach the asker regardless of circle, because the thread was established at ask-time.
        </p>
        <Example>
{`ack("ask-c1a1c7dd")
ack("ask-c1a1c7dd", "we expose /health, /peers, /ask, /ack")`}
        </Example>
      </Tool>

      <Tool
        name="notify_peer"
        signature={`notify_peer(peer_name: str, message: str, circle: str | None = None, attachments: list[dict] | None = None) -> str`}
      >
        <p>
          Fire-and-forget. No lifecycle, no expected response. Returns a synthetic <Mono>notif-XXXXXXXX</Mono> ID for client-side tracking, not a thread you can close. Use for status pings and announcements.
        </p>
        <p>
          On the HTTP <Mono>/notify</Mono> response, <Mono>hook_delivery</Mono> may be present when the recipient is a new enough WebSocket hook. It is a best-effort terminal injection receipt with statuses such as <Mono>injected</Mono>, <Mono>rejected</Mono>, or <Mono>failed</Mono>; <Mono>null</Mono> means the hook is older, a non-hook transport handled the notify, or no receipt arrived before the daemon returned. When a session binding is known, notify responses and hook receipts may include nullable <Mono>repowire_session_id</Mono>, <Mono>from_repowire_session_id</Mono>, and <Mono>to_repowire_session_id</Mono> fields for grouping.
        </p>
        <p>
          If the live transport is unavailable but the daemon can resolve the target peer, <Mono>/notify</Mono> may return <Mono>delivery_state=&quot;queued&quot;</Mono> and <Mono>reason=&quot;queued_delivery&quot;</Mono>. The notification is then stored for one Stop-hook or <Mono>repowire peer deliveries</Mono> drain, subject to the configured TTL and per-peer cap.
        </p>
        <p>
          The special peer <Mono>telegram</Mono> routes to the user&rsquo;s phone. The <Mono>dashboard</Mono> already sees agent turns; you do not need to notify it.
        </p>
        <Example>
{`notify_peer("telegram", "deploy finished, green across CI")`}
        </Example>
      </Tool>

      <Tool name="broadcast" signature={`broadcast(message: str) -> str`}>
        <p>
          Fan out to every online peer in your circle. No correlation, no reply. Use sparingly; treat it as a soft interrupt for everyone in scope.
        </p>
        <Example>
{`broadcast("rebasing main, hold pushes for ~5 min")`}
        </Example>
      </Tool>

      <Tool
        name="list_peers"
        signature={`list_peers(show_offline: bool = False, include_self: bool = False) -> str`}
      >
        <p>
          Returns a TSV with columns: <Mono>peer_id, name, project, circle, role, status, path, machine, description, backend, last_seen, turn_state</Mono>. The <Mono>turn_state</Mono> column is empty when unknown; otherwise <Mono>idle</Mono>, <Mono>working</Mono>, <Mono>awaiting_input</Mono> (peer is mid-turn waiting on user input), or <Mono>pending_first_turn</Mono> (spawn-seeded peer whose first prompt never landed). Defaults to online + busy peers and hides the caller. Pass <Mono>show_offline=True</Mono> for the full registry; pass <Mono>include_self=True</Mono> when an orchestrator needs its own row.
        </p>
      </Tool>

      <Tool name="whoami" signature={`whoami() -> str`}>
        <p>
          Returns the caller&rsquo;s own TSV row. Useful when an agent needs to know which display name it is registered under (display names get suffixed on collision: <Mono>repowire</Mono>, <Mono>repowire-2</Mono>).
        </p>
      </Tool>

      <Tool name="set_description" signature={`set_description(description: str) -> str`}>
        <p>
          Update the free-form description visible in <Mono>list_peers</Mono>. Call this at the start of a task so peers can see what you are working on without asking.
        </p>
        <Example>{`set_description("rebuilding docs slice B")`}</Example>
      </Tool>

      <Tool
        name="job_create"
        signature={`job_create(title: str = "", kind: str = "general", assigned_peer_id: str | None = None, owner_peer_id: str | None = None, repowire_session_id: str | None = None, correlation_id: str | None = None, circle: str | None = None, source_kind: str | None = None, source_id: str | None = None, scope: str | None = None, visibility: str = "circle", request: dict | None = None, deadline_at: str | None = None, expires_at: str | None = None, provenance: dict | None = None) -> str`}
      >
        <p>
          Create a durable tracked work job through the daemon <Mono>/jobs</Mono> API. Returns JSON with <Mono>job_id</Mono>, <Mono>work_id</Mono>, and <Mono>status</Mono>. The caller&rsquo;s peer ID is sent as <Mono>created_by_peer_id</Mono> when available.
        </p>
      </Tool>

      <Tool
        name="job_list"
        signature={`job_list(state: str | None = None, owner_peer_id: str | None = None, created_by_peer_id: str | None = None, repowire_session_id: str | None = None, circle: str | None = None) -> str`}
      >
        <p>
          List durable jobs through <Mono>/jobs</Mono>. Returns JSON shaped like <Mono>{`{"work": [status, ...]}`}</Mono>. Filters mirror the HTTP API.
        </p>
      </Tool>

      <Tool
        name="job_status / job_show"
        signature={`job_status(job_id: str) -> str
job_show(job_id: str) -> str`}
      >
        <p>
          Return one job&rsquo;s current status JSON. <Mono>job_show</Mono> is an alias for <Mono>job_status</Mono>.
        </p>
      </Tool>

      <Tool
        name="job_update"
        signature={`job_update(job_id: str, state: str, state_reason: str | None = None, phase: str | None = None, progress: dict | None = None, progress_note: str | None = None, result_summary: str | None = None, result_data: dict | None = None, error: dict | None = None, artifacts: list | None = None, provenance: dict | None = None) -> str`}
      >
        <p>
          Update a job lifecycle state through <Mono>PATCH /jobs/{"{job_id}"}</Mono>. Returns the updated status JSON. Terminal jobs cannot move back to non-terminal states.
        </p>
      </Tool>

      <Tool name="job_result" signature={`job_result(job_id: str) -> str`}>
        <p>
          Return terminal result JSON for a job, or <Mono>result_state=&quot;not_ready&quot;</Mono> with the current status while the job is non-terminal.
        </p>
      </Tool>

      <Tool name="job_cancel" signature={`job_cancel(job_id: str, reason: str = "cancel_requested") -> str`}>
        <p>
          Request cancellation for a tracked work job. Queued jobs move directly to <Mono>cancelled</Mono>; running, delivered, awaiting-input, or blocked jobs record <Mono>cancel_requested</Mono> and remain pending until an executor reports a terminal state.
        </p>
        <p>
          When the daemon already owns a live ACP session for the job&rsquo;s assigned peer, it attempts a bounded protocol <Mono>session/cancel</Mono> and reports the result in <Mono>status.protocol_cancel</Mono>. If there is no live session or execution link, <Mono>protocol_cancel</Mono> reports <Mono>unavailable</Mono> rather than claiming runtime cancellation.
        </p>
      </Tool>

      <Tool
        name="spawn_peer"
        signature={`spawn_peer(path: str, backend: str, profile: str | None = None, circle: str | None = None, message: str | None = None) -> str`}
      >
        <p>
          Spawn a new agent session in a project directory. The <Mono>backend</Mono> must have a launch profile in <Mono>daemon.spawn.commands</Mono> in <Mono>~/.repowire/config.yaml</Mono>; spawn is off by default until you configure a backend and allowed path. Pass <Mono>profile</Mono> to append args from <Mono>daemon.spawn.profiles</Mono> for model/profile selection.
        </p>
        <p>
          If <Mono>circle</Mono> is omitted, the tool uses the caller&apos;s current circle. Pass <Mono>circle=&quot;default&quot;</Mono> explicitly to target the <Mono>default</Mono> tmux session.
        </p>
        <p>
          The spawned agent self-registers via its SessionStart hook within a few seconds. The <Mono>message</Mono> seeds first-turn context. Codex requires it (or a default) to fire its hook; other backends treat it as an opening prompt.
        </p>
      </Tool>

      <Tool
        name="kill_peer"
        signature={`kill_peer(peer_identifier: str, circle: str | None = None) -> str`}
      >
        <p>
          Terminate a peer by name or peer_id. Used by orchestrators to reclaim slots when a session is stuck or done. The daemon marks the peer offline reliably and attempts to reap its tmux pane (best-effort; verify with <Mono>tmux list-windows</Mono> and follow up with <Mono>tmux kill-window</Mono> if the pane survives).
        </p>
      </Tool>

      <Tool
        name="schedule_create"
        signature={`schedule_create(to_peer: str, text: str, fire_at: str, kind: str = "notify", circle: str | None = None) -> str`}
      >
        <p>
          Schedule a one-shot future message to another peer. <Mono>fire_at</Mono> is ISO-8601; naive datetimes are interpreted as UTC. Use <Mono>kind=&quot;ask&quot;</Mono> when the future message should open an ask thread.
        </p>
      </Tool>

      <Tool
        name="schedule_self"
        signature={`schedule_self(text: str, fire_at: str | None = None, cron: str | None = None, kind: str = "notify", circle: str | None = None) -> str`}
      >
        <p>
          Schedule a future message to yourself. Provide exactly one of <Mono>fire_at</Mono> or <Mono>cron</Mono>. Cron accepts five-field expressions and aliases such as <Mono>@hourly</Mono>, <Mono>@daily</Mono>, <Mono>@midnight</Mono>, <Mono>@weekly</Mono>, and <Mono>@monthly</Mono>.
        </p>
      </Tool>

      <Tool
        name="schedule_cron"
        signature={`schedule_cron(to_peer: str, text: str, cron: str, kind: str = "notify", circle: str | None = None) -> str`}
      >
        <p>
          Schedule a recurring message to another peer. Recurring schedules advance to their next fire time after delivery and keep running until cancelled with <Mono>schedule_delete</Mono>.
        </p>
      </Tool>

      <Tool
        name="schedule_list"
        signature={`schedule_list(mine_only: bool = True, include_cron: bool = False) -> str`}
      >
        <p>
          List pending schedules as TSV. Pass <Mono>mine_only=False</Mono> for all schedules on the daemon, and <Mono>include_cron=True</Mono> to append the recurrence column.
        </p>
      </Tool>

      <Tool name="schedule_delete" signature={`schedule_delete(schedule_id: str) -> str`}>
        <p>
          Cancel a one-shot or recurring schedule by the <Mono>sched-XXXXXXXX</Mono> id returned when it was created.
        </p>
      </Tool>

      <div className="mt-12 border-t border-border-faint pt-8">
        <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
          See also
        </div>
        <p className="text-sm leading-6 text-on-surface-variant">
          The <a className="text-primary-fixed underline-offset-4 hover:underline" href="/docs/reference/client">typed Python client</a> exposes the same calls over the daemon&rsquo;s HTTP API for non-MCP callers.
        </p>
      </div>
    </article>
  );
}

function Tool({
  name,
  signature,
  children,
}: {
  name: string;
  signature: string;
  children: React.ReactNode;
}) {
  return (
    <section className="mt-10">
      <h2 className="font-mono text-base font-semibold text-primary-fixed">{name}</h2>
      <pre className="mt-3 overflow-x-auto border border-border-faint bg-surface-container-low p-3 font-mono text-xs leading-6 text-on-surface">
        <code>{signature}</code>
      </pre>
      <div className="mt-4 space-y-4 text-sm leading-6 text-on-surface-variant">{children}</div>
    </section>
  );
}

function Example({ children }: { children: React.ReactNode }) {
  return (
    <pre className="overflow-x-auto border-l-2 border-primary/60 bg-surface-container-low p-4 font-mono text-xs leading-6 text-on-surface">
      <code>{children}</code>
    </pre>
  );
}

function Mono({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-primary-fixed">{children}</code>;
}
