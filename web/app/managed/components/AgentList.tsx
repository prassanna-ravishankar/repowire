import { Bot, Plus } from "lucide-react";
import { cn } from "../../dashboard/lib/utils";
import type { Agent } from "../lib/api";
import { agentLabel, agentSubline } from "../lib/state";
import { EmptyState, SkeletonRows, StateDot, StateLabel, primaryButtonClass } from "./ui";

export function AgentList({
  agents,
  total,
  retiredCount,
  loading,
  selected,
  showRetired,
  now,
  onShowRetired,
  onSelect,
  onCreate,
}: {
  agents: Agent[];
  total: number;
  retiredCount: number;
  loading: boolean;
  selected: string | null;
  showRetired: boolean;
  now: number;
  onShowRetired: (value: boolean) => void;
  onSelect: (id: string) => void;
  onCreate: () => void;
}) {
  return (
    <aside className="flex h-full min-h-0 flex-col bg-surface-dim">
      <div className="flex items-center gap-3 border-b border-border-faint px-4 py-2.5">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.2em] text-outline">
          Agents <span className="ml-1 tabular-nums text-on-surface-variant">{total - retiredCount}</span>
        </span>
        {retiredCount > 0 && (
          <label className="ml-auto inline-flex cursor-pointer items-center gap-1.5 font-mono text-[10px] text-outline">
            <input type="checkbox" checked={showRetired} onChange={(event) => onShowRetired(event.target.checked)} className="accent-[var(--accent)]" />
            show retired ({retiredCount})
          </label>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {loading ? (
          <SkeletonRows />
        ) : agents.length === 0 ? (
          <EmptyState icon={<Bot className="h-6 w-6" />} title="No managed agents">
            <p>
              Create one here, or from any mesh session with <code className="text-on-surface-variant">spawn_peer(backend=&quot;hosted&quot;)</code>.
            </p>
            <button onClick={onCreate} className={cn(primaryButtonClass, "mt-4")}>
              <Plus className="h-3.5 w-3.5" /> New agent
            </button>
          </EmptyState>
        ) : (
          agents.map((agent) => <AgentRow key={agent.id} agent={agent} active={agent.id === selected} now={now} onClick={() => onSelect(agent.id)} />)
        )}
      </div>
    </aside>
  );
}

function AgentRow({ agent, active, now, onClick }: { agent: Agent; active: boolean; now: number; onClick: () => void }) {
  const retired = agent.state === "retired";
  return (
    <button
      onClick={onClick}
      aria-current={active ? "true" : undefined}
      className={cn(
        "block w-full border-b border-l-2 border-b-border-faint py-2.5 pl-3 pr-4 text-left transition-colors",
        active ? "border-l-primary bg-primary/10" : "border-l-transparent hover:bg-surface-container",
        retired && "opacity-60",
      )}
    >
      <div className="flex min-w-0 items-center gap-2.5">
        <StateDot state={agent.state} />
        <span className={cn("min-w-0 flex-1 truncate font-mono text-[13px] font-semibold", active ? "text-primary-fixed" : "text-on-surface", retired && "line-through")}>
          {agentLabel(agent)}
        </span>
        <StateLabel state={agent.state} />
      </div>
      <div className={cn("ml-[18px] truncate font-mono text-[11px] leading-5 tabular-nums", agent.state === "failed" ? "text-error" : "text-outline")}>
        {agentSubline(agent, now)}
      </div>
      <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-outline">
        {[agent.workspace ? `ws ${agent.workspace}` : "no workspace", agent.home_machine ?? "managed"].join(" · ")}
      </div>
    </button>
  );
}
