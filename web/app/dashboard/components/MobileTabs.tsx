import { cn } from "../lib/utils";

export type MobileTab = "peers" | "mesh";

export function MobileTabs({
  activeTab,
  counts,
  eventCount,
  onChange,
}: {
  activeTab: MobileTab;
  counts: Record<"online" | "busy" | "offline", number>;
  eventCount: number;
  onChange: (tab: MobileTab) => void;
}) {
  return (
    <nav className="col-span-full flex border-t border-border-faint bg-surface-dim md:hidden">
      <MobileTabButton active={activeTab === "peers"} label="PEERS" sub={`${counts.online} online · ${counts.busy} busy`} onClick={() => onChange("peers")} />
      <MobileTabButton active={activeTab === "mesh"} label="MESH" sub={`${eventCount} events`} onClick={() => onChange("mesh")} />
    </nav>
  );
}

function MobileTabButton({ active, label, sub, onClick }: { active: boolean; label: string; sub: string; onClick: () => void }) {
  return (
    <button onClick={onClick} className={cn("flex flex-1 flex-col items-center gap-1 border-t-2 px-3 py-2", active ? "border-primary text-primary-fixed" : "border-transparent text-outline")}>
      <span className="font-mono text-[11px] font-semibold tracking-[0.18em]">{label}</span>
      <span className="font-mono text-[10px]">{sub}</span>
    </button>
  );
}
