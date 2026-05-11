import { useMemo } from "react";
import { Search } from "lucide-react";
import { cn, shortPath, statusDot } from "../lib/utils";
import type { Peer } from "../types";
import { peerLabel } from "../types";
import { StatusLabel, statusRank } from "./status";

export function PeerRoster({
  peers,
  allCount,
  selectedPeerId,
  filter,
  onFilter,
  onSelectPeer,
}: {
  peers: Peer[];
  allCount: number;
  selectedPeerId: string | null;
  filter: string;
  onFilter: (value: string) => void;
  onSelectPeer: (peer: Peer) => void;
}) {
  const byCircle = useMemo(() => {
    const grouped = new Map<string, Peer[]>();
    for (const peer of peers) {
      const circle = peer.circle || "default";
      grouped.set(circle, [...(grouped.get(circle) ?? []), peer]);
    }
    for (const list of grouped.values()) {
      list.sort((a, b) => statusRank(a.status) - statusRank(b.status) || peerLabel(a).localeCompare(peerLabel(b)));
    }
    return Array.from(grouped.entries()).sort(([a], [b]) => a.localeCompare(b));
  }, [peers]);

  return (
    <aside className="flex min-h-0 flex-col bg-surface-dim md:h-full md:overflow-hidden">
      <div className="sticky top-[var(--topbar-offset)] z-20 flex items-center gap-2 border-b border-border-faint bg-surface-dim px-3 py-2 md:static">
        <Search className="h-3.5 w-3.5 shrink-0 text-outline" />
        <input
          value={filter}
          onChange={(event) => onFilter(event.target.value)}
          placeholder="filter peers, circles, paths..."
          className="min-w-0 flex-1 bg-transparent font-mono text-xs text-on-surface outline-none placeholder:text-outline"
        />
        <span className="font-mono text-[10px] text-outline tabular-nums">{peers.length}/{allCount}</span>
      </div>

      <div className="min-h-0 flex-1 md:overflow-y-auto">
        {byCircle.map(([circle, list]) => (
          <section key={circle}>
            <div className="flex items-baseline justify-between px-3.5 pb-1.5 pt-3 font-mono text-[9px] font-semibold uppercase tracking-[0.2em] text-outline">
              <span>circle / {circle}</span>
              <span>{list.length}</span>
            </div>
            {list.map((peer) => (
              <PeerRow
                key={peer.peer_id}
                peer={peer}
                active={peer.peer_id === selectedPeerId}
                onClick={() => onSelectPeer(peer)}
              />
            ))}
          </section>
        ))}
        {peers.length === 0 && (
          <div className="px-4 py-12 text-center font-mono text-xs leading-6 text-outline">
            <div className="mb-1 text-on-surface-variant">&gt; no peers match</div>
            try a wider filter or start an agent session
          </div>
        )}
      </div>
    </aside>
  );
}

function PeerRow({ peer, active, onClick }: { peer: Peer; active: boolean; onClick: () => void }) {
  const { folder, parent } = peer.path ? shortPath(peer.path) : { folder: "", parent: "" };
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "block w-full border-b border-border-faint border-l-2 px-3 py-2.5 text-left transition-colors",
        active
          ? "border-l-primary bg-primary/10 text-primary-fixed"
          : "border-l-transparent text-on-surface hover:bg-surface-container"
      )}
    >
      <div className="mb-1 flex min-w-0 items-center gap-2.5">
        <span className={cn("h-2 w-2 shrink-0 rounded-full", statusDot(peer.status))} />
        <span className="min-w-0 flex-1 truncate font-mono text-[13px] font-semibold">{peerLabel(peer)}</span>
        <StatusLabel status={peer.status} />
      </div>
      <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-outline">
        {peer.backend || "agent"} · {peer.metadata?.branch ? String(peer.metadata.branch) : peer.circle}
      </div>
      {peer.path && (
        <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-outline">
          {parent}<span className="text-on-surface-variant">{folder}</span>
        </div>
      )}
      {peer.description && (
        <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-tertiary-fixed-dim">
          ↳ {peer.description}
        </div>
      )}
    </button>
  );
}
