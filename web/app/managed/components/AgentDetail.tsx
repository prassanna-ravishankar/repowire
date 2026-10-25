import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { Archive, ChevronLeft, HardDrive, Send, Square } from "lucide-react";
import { cn } from "../../dashboard/lib/utils";
import { managedApi, type Agent, type Run, type Workspace } from "../lib/api";
import type { Outgoing } from "../lib/composer";
import type { AgentTab } from "../lib/hashRoute";
import { agentLabel, agentSubline, runStatus, formatAgo, formatDate, formatDuration, formatTokens, formatUsd, formatWhen } from "../lib/state";
import { parseTranscript, type TranscriptItem } from "../lib/transcript";
import { TranscriptView } from "./Transcript";
import { CopyId, EmptyState, InlineError, SkeletonRows, StateDot, StateLabel, buttonClass, labelClass } from "./ui";

const TABS: { id: AgentTab; label: string; phone: string }[] = [
  { id: "chat", label: "Chat", phone: "Chat" },
  { id: "runs", label: "Runs", phone: "Runs" },
  { id: "workspace", label: "Workspace", phone: "Files" },
];

export function AgentDetail({
  agent,
  tab,
  workspace,
  runs,
  outgoing,
  now,
  onTab,
  onBack,
  onSend,
  onRetry,
  onTranscript,
  onLoadRuns,
  onStop,
  onRetire,
  onCopied,
}: {
  agent: Agent;
  tab: AgentTab;
  workspace: Workspace | null;
  runs: Run[] | undefined;
  outgoing: Outgoing[];
  now: number;
  onTab: (tab: AgentTab) => void;
  onBack: () => void;
  onSend: (text: string) => void;
  onRetry: (message: Outgoing) => void;
  onTranscript: (items: TranscriptItem[]) => void;
  onLoadRuns: () => Promise<void>;
  onStop: () => Promise<void>;
  onRetire: () => void;
  onCopied: () => void;
}) {
  const [stopping, setStopping] = useState(false);
  const working = agent.state === "working";
  const retired = agent.state === "retired";

  const stop = async () => {
    setStopping(true);
    try {
      await onStop();
    } finally {
      setStopping(false);
    }
  };

  return (
    <section className="flex min-h-0 flex-1 flex-col">
      <header className="border-b border-border-faint bg-surface-dim px-4 pb-0 pt-3 md:px-6">
        <div className="flex items-start gap-2">
          <button onClick={onBack} aria-label="Back to agents" className="-ml-1 mt-0.5 text-outline hover:text-on-surface md:hidden">
            <ChevronLeft className="h-5 w-5" />
          </button>
          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
              <StateDot state={agent.state} />
              <h1 className={cn("truncate font-mono text-base font-semibold text-on-surface", retired && "line-through")}>{agentLabel(agent)}</h1>
              <StateLabel state={agent.state} />
              <span className={cn("font-mono text-[11px] tabular-nums", agent.state === "failed" ? "text-error" : "text-outline")}>{agentSubline(agent, now)}</span>
            </div>
            <div className="mt-1 flex flex-wrap gap-x-3 font-mono text-[11px] text-outline">
              {agent.display_name && <CopyId value={agent.id} onCopied={onCopied} />}
              <span>{agent.workspace ? `ws ${agent.workspace}` : "no workspace"}</span>
              <span title="Estimated from metered tokens and container time">est. {formatUsd(agent.cost_usd)}</span>
              <span className="hidden sm:inline">{formatTokens(agent.tokens_in + agent.tokens_out)} tokens</span>
              <span className="hidden sm:inline">created {formatDate(agent.created_at)} by {agent.created_by}</span>
              <span>{agent.home_machine ? `home ${agent.home_machine}` : "not on mesh"}</span>
            </div>
            {agent.description && <p className="mt-1 truncate font-mono text-[11px] text-tertiary-fixed-dim">↳ {agent.description}</p>}
          </div>
          <div className="flex shrink-0 gap-2">
            {working && (
              <button onClick={stop} disabled={stopping} className={buttonClass} title="Abort the current run">
                <Square className="h-3 w-3" /> <span className="hidden sm:inline">{stopping ? "Stopping" : "Stop"}</span>
              </button>
            )}
            {!retired && (
              <button onClick={onRetire} className={buttonClass} title="Retire this agent">
                <Archive className="h-3.5 w-3.5" /> <span className="hidden sm:inline">Retire</span>
              </button>
            )}
          </div>
        </div>
        <nav className="-mb-px mt-3 flex gap-5" role="tablist">
          {TABS.map((t) => (
            <button
              key={t.id}
              role="tab"
              aria-selected={tab === t.id}
              onClick={() => onTab(t.id)}
              className={cn(
                "border-b-2 pb-2 font-mono text-[10px] font-bold uppercase tracking-[0.16em] transition-colors",
                tab === t.id ? "border-primary text-primary" : "border-transparent text-outline hover:text-on-surface",
              )}
            >
              <span className="hidden sm:inline">{t.label}</span>
              <span className="sm:hidden">{t.phone}</span>
            </button>
          ))}
        </nav>
      </header>
      {tab === "chat" && <ChatTab agent={agent} outgoing={outgoing} onSend={onSend} onRetry={onRetry} onTranscript={onTranscript} />}
      {tab === "runs" && <RunsTab runs={runs} now={now} onLoad={onLoadRuns} onCopied={onCopied} />}
      {tab === "workspace" && <WorkspaceTab agent={agent} workspace={workspace} now={now} />}
    </section>
  );
}

function ChatTab({
  agent,
  outgoing,
  onSend,
  onRetry,
  onTranscript,
}: {
  agent: Agent;
  outgoing: Outgoing[];
  onSend: (text: string) => void;
  onRetry: (message: Outgoing) => void;
  onTranscript: (items: TranscriptItem[]) => void;
}) {
  const [items, setItems] = useState<TranscriptItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reload, setReload] = useState(0);
  const scroller = useRef<HTMLDivElement>(null);
  const onTranscriptRef = useRef(onTranscript);
  useEffect(() => {
    onTranscriptRef.current = onTranscript;
  }, [onTranscript]);

  // Reload when the agent's state changes (admission and settle both push an
  // agent frame), never on a timer.
  useEffect(() => {
    let cancelled = false;
    managedApi.messages(agent.id).then(
      (raw) => {
        if (cancelled) return;
        const parsed = parseTranscript(raw);
        setItems(parsed);
        setError(null);
        onTranscriptRef.current(parsed);
      },
      (err: Error) => !cancelled && setError(err.message),
    );
    return () => {
      cancelled = true;
    };
  }, [agent.id, agent.state, agent.state_at, reload]);

  const count = (items?.length ?? 0) + outgoing.length;
  useEffect(() => {
    scroller.current?.scrollTo({ top: scroller.current.scrollHeight });
  }, [count, agent.id]);

  return (
    <>
      <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto">
        {error && <InlineError className="m-4" error={error} onRetry={() => setReload((n) => n + 1)} />}
        {items === null && !error ? (
          <SkeletonRows />
        ) : items && items.length === 0 && outgoing.length === 0 ? (
          <EmptyState icon={<Send className="h-6 w-6" />} title="No messages yet">
            Say hello. Runs start when a message arrives.
          </EmptyState>
        ) : (
          <TranscriptView items={items ?? []} agentName={agentLabel(agent)} outgoing={outgoing} working={agent.state === "working"} onRetry={onRetry} />
        )}
      </div>
      <Composer agent={agent} onSend={onSend} />
    </>
  );
}

export function Composer({ agent, onSend }: { agent: Agent; onSend: (text: string) => void }) {
  const [text, setText] = useState("");
  const retired = agent.state === "retired";
  const submit = () => {
    const value = text.trim();
    if (!value || retired) return;
    onSend(value);
    setText("");
  };
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      submit();
    }
  };
  return (
    <div className="border-t border-border-faint bg-surface-dim px-3 pb-[max(env(safe-area-inset-bottom),0.75rem)] pt-3 md:px-6">
      {retired ? (
        <p className="py-2 text-center font-mono text-[11px] text-outline">Retired agents do not accept messages. The transcript stays readable.</p>
      ) : (
        <div className="flex items-end gap-2">
          <textarea
            value={text}
            onChange={(event) => setText(event.target.value)}
            onKeyDown={onKeyDown}
            rows={Math.min(6, Math.max(1, text.split("\n").length))}
            placeholder={`Message ${agentLabel(agent)}`}
            aria-label={`Message ${agentLabel(agent)}`}
            className="min-h-10 flex-1 resize-none rounded border border-border bg-surface-container-lowest px-3 py-2 text-base text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary md:text-sm"
          />
          <button onClick={submit} disabled={!text.trim()} aria-label="Send" className="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded bg-primary text-on-primary transition-[filter] hover:brightness-110 disabled:opacity-40">
            <Send className="h-4 w-4" />
          </button>
        </div>
      )}
    </div>
  );
}

function runDuration(run: Run, now: number) {
  return formatDuration((run.settled_at ? Date.parse(run.settled_at) : now) - Date.parse(run.started_at));
}

function RunsTab({ runs, now, onLoad, onCopied }: { runs: Run[] | undefined; now: number; onLoad: () => Promise<void>; onCopied: () => void }) {
  const [error, setError] = useState<string | null>(null);
  const load = () => onLoad().then(() => setError(null), (err: Error) => setError(err.message));
  // Fetch whenever the list is missing: on open, and after a snapshot invalidates it.
  const missing = runs === undefined;
  useEffect(() => {
    if (missing) void load();
  }, [missing]); // eslint-disable-line react-hooks/exhaustive-deps

  if (error) return <InlineError className="m-4" error={error} onRetry={load} />;
  if (!runs) return <SkeletonRows />;
  if (runs.length === 0) return <EmptyState icon={<Square className="h-6 w-6" />} title="No runs yet" />;
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <table className="hidden w-full font-mono text-[11px] md:table">
        <thead>
          <tr className={cn(labelClass, "text-left")}>
            {["Started", "Op", "From", "Duration", "Status", "Tokens", "Est."].map((h, i) => (
              <th key={h} className={cn("border-b border-border-faint px-3 py-2 font-semibold first:pl-6", i >= 5 && "text-right last:pr-6")}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <RunRow key={run.op} run={run} now={now} onCopied={onCopied} />
          ))}
        </tbody>
      </table>
      <ul className="md:hidden">
        {runs.map((run) => (
          <li key={run.op} className="border-b border-border-faint px-4 py-2.5 font-mono text-[11px]">
            <div className="flex items-baseline gap-2">
              <span className={cn("font-semibold tracking-[0.12em]", runStatus(run).cls)}>{runStatus(run).label}</span>
              <span className="text-outline">{runDuration(run, now)}</span>
              <span className="ml-auto text-outline">{formatWhen(run.started_at, now)}</span>
            </div>
            <div className="mt-0.5 truncate text-outline">
              {run.source ?? "unknown"} · <CopyId value={run.op} display={run.op.slice(0, 12)} onCopied={onCopied} />
              {run.status !== "working" && ` · ${formatTokens(run.tokens_in + run.tokens_out)} tok · ${formatUsd(run.cost_usd)}`}
            </div>
            {runStatus(run).failure && <div className="mt-0.5 text-error">{run.reason}</div>}
          </li>
        ))}
      </ul>
    </div>
  );
}

function RunRow({ run, now, onCopied }: { run: Run; now: number; onCopied: () => void }) {
  const settled = run.status !== "working";
  return (
    <>
      <tr className="text-on-surface-variant">
        <td className={cn("px-3 py-2 pl-6 tabular-nums", !runStatus(run).failure && "border-b border-border-faint")}>{formatWhen(run.started_at, now)}</td>
        <td className={cn("px-3 py-2", !runStatus(run).failure && "border-b border-border-faint")}>
          <CopyId value={run.op} display={run.op.length > 14 ? `${run.op.slice(0, 12)}…` : run.op} onCopied={onCopied} />
        </td>
        <td className={cn("max-w-48 truncate px-3 py-2", !runStatus(run).failure && "border-b border-border-faint")}>{run.source ?? "-"}</td>
        <td className={cn("px-3 py-2 tabular-nums", !runStatus(run).failure && "border-b border-border-faint")}>{runDuration(run, now)}</td>
        <td className={cn("px-3 py-2 font-semibold tracking-[0.12em]", runStatus(run).cls, !runStatus(run).failure && "border-b border-border-faint")}>{runStatus(run).label}</td>
        <td className={cn("px-3 py-2 text-right tabular-nums", !runStatus(run).failure && "border-b border-border-faint")}>{settled ? formatTokens(run.tokens_in + run.tokens_out) : "-"}</td>
        <td className={cn("px-3 py-2 pr-6 text-right tabular-nums", !runStatus(run).failure && "border-b border-border-faint")}>{settled ? formatUsd(run.cost_usd) : "-"}</td>
      </tr>
      {runStatus(run).failure && (
        <tr>
          <td colSpan={7} className="border-b border-border-faint px-3 pb-2 pl-6 text-error">{run.reason}</td>
        </tr>
      )}
    </>
  );
}

const WORKSPACE_STATE: Record<Workspace["state"], { label: string; cls: string }> = {
  idle: { label: "STOPPED", cls: "text-outline" },
  running: { label: "RUNNING", cls: "text-secondary" },
  needs_repair: { label: "NEEDS REPAIR", cls: "text-error" },
};

// Container snapshots are retained for 30 days; any command refreshes them.
const SNAPSHOT_TTL_DAYS = 30;

function WorkspaceTab({ agent, workspace, now }: { agent: Agent; workspace: Workspace | null; now: number }) {
  if (!agent.workspace) {
    return (
      <EmptyState icon={<HardDrive className="h-6 w-6" />} title="No workspace">
        This agent has no shell. Workspace attachment is chosen at creation and cannot be changed later.
      </EmptyState>
    );
  }
  if (!workspace) return <EmptyState icon={<HardDrive className="h-6 w-6" />} title={agent.workspace}>No commands have run here yet.</EmptyState>;
  const snapshotDays = workspace.snapshot_at ? (now - Date.parse(workspace.snapshot_at)) / 86_400_000 : null;
  const expiresIn = snapshotDays === null ? null : Math.max(0, Math.ceil(SNAPSHOT_TTL_DAYS - snapshotDays));
  const others = workspace.agents.filter((id) => id !== agent.id);
  const rows: [string, React.ReactNode][] = [
    ["State", <span key="s" className={cn("font-semibold tracking-[0.12em]", WORKSPACE_STATE[workspace.state].cls)}>{WORKSPACE_STATE[workspace.state].label}</span>],
    ["Snapshot", workspace.snapshot_at ? `${formatAgo(workspace.snapshot_at, now)} (expires in ${expiresIn} days)` : "none"],
    ["Shared by", workspace.agents.length ? workspace.agents.join(", ") : "-"],
    ["Commands", `${workspace.exec_count} total · last ${formatWhen(workspace.last_exec_at, now)}`],
    ["Container", formatDuration(workspace.container_ms)],
    ["Limits", "no network · 45s per command"],
  ];
  return (
    <div className="min-h-0 flex-1 overflow-y-auto p-4 md:p-6">
      <div className="max-w-2xl rounded-[8px] border border-border bg-surface-container-low">
        <div className="border-b border-border-faint px-4 py-3 font-mono text-sm font-semibold text-on-surface">{workspace.id}</div>
        {workspace.state === "needs_repair" && (
          <InlineError className="m-4" error="Workspace stopped before checkpointing. Commands are refused until repaired." />
        )}
        {expiresIn !== null && expiresIn <= 5 && workspace.state !== "needs_repair" && (
          <p className="mx-4 mt-4 border border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 px-3 py-2 font-mono text-[11px] text-on-tertiary-container">
            Snapshot expires in {expiresIn} days; any command refreshes it.
          </p>
        )}
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 px-4 py-4 font-mono text-[12px]">
          {rows.map(([label, value]) => (
            <div key={label} className="contents">
              <dt className={labelClass}>{label}</dt>
              <dd className="text-on-surface-variant">{value}</dd>
            </div>
          ))}
        </dl>
        {others.length > 0 && <p className="border-t border-border-faint px-4 py-2 font-mono text-[11px] text-outline">Files here are visible to {others.join(", ")}.</p>}
      </div>
    </div>
  );
}
