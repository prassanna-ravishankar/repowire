"use client";

import { useMemo, useState } from "react";
import { ChevronRight } from "lucide-react";
import { cn, statusDot, timeAgo } from "../lib/utils";
import type { Peer } from "../types";

interface SidebarProps {
  peers: Peer[];
  selectedPeerId: string | null;
  onSelectPeer: (peer: Peer) => void;
}

export function Sidebar({ peers, selectedPeerId, onSelectPeer }: SidebarProps) {
  const [offlineExpanded, setOfflineExpanded] = useState(false);

  const { active, offline } = useMemo(() => {
    const active: Peer[] = [];
    const offline: Peer[] = [];

    for (const p of peers) {
      if (p.status === "online" || p.status === "busy") {
        active.push(p);
      } else {
        offline.push(p);
      }
    }

    // Sort: online first, then busy; stable tie-break by name
    active.sort((a, b) => {
      const s = (a.status === "online" ? 0 : 1) - (b.status === "online" ? 0 : 1);
      return s !== 0 ? s : a.name.localeCompare(b.name);
    });
    offline.sort((a, b) => a.name.localeCompare(b.name));

    return { active, offline };
  }, [peers]);

  const renderPeerRow = (peer: Peer) => {
    const isSelected = peer.peer_id === selectedPeerId;

    return (
      <li key={peer.peer_id}>
        <button
          onClick={() => onSelectPeer(peer)}
          className={cn(
            "w-full flex items-center gap-2 px-2.5 py-2 rounded-md text-left transition-colors",
            isSelected
              ? "bg-zinc-800 text-zinc-200"
              : "hover:bg-zinc-900 text-zinc-400"
          )}
        >
          <span className={cn("w-2 h-2 rounded-full shrink-0", statusDot(peer.status))} />
          <span className="text-sm font-medium truncate">{peer.name}</span>
          <span className="text-[10px] px-1.5 py-0.5 rounded bg-zinc-800 text-zinc-500 font-mono shrink-0">
            {peer.circle}
          </span>
        </button>
      </li>
    );
  };

  return (
    <aside className="w-56 border-r border-zinc-800 flex flex-col overflow-y-auto shrink-0">
      {/* Active peers section */}
      <div className="px-3 pt-3 pb-1">
        <span className="text-[10px] font-mono text-zinc-600 uppercase tracking-wider">
          Active ({active.length})
        </span>
      </div>

      {active.length === 0 ? (
        <p className="text-xs text-zinc-600 px-3 py-2">No peers online</p>
      ) : (
        <ul className="flex flex-col gap-0.5 px-2 pb-2">
          {active.map(renderPeerRow)}
        </ul>
      )}

      {/* Offline section - collapsible */}
      {offline.length > 0 && (
        <div className="border-t border-zinc-800/50">
          <button
            onClick={() => setOfflineExpanded(!offlineExpanded)}
            className="w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-zinc-900/50 transition-colors"
          >
            <ChevronRight
              className={cn(
                "w-3 h-3 text-zinc-600 transition-transform",
                offlineExpanded && "rotate-90"
              )}
            />
            <span className="text-[10px] font-mono text-zinc-600 uppercase tracking-wider">
              Offline ({offline.length})
            </span>
          </button>

          {offlineExpanded && (
            <ul className="flex flex-col gap-0.5 px-2 pb-2">
              {offline.map((peer) => {
                const isSelected = peer.peer_id === selectedPeerId;
                return (
                  <li key={peer.peer_id}>
                    <button
                      onClick={() => onSelectPeer(peer)}
                      className={cn(
                        "w-full flex items-center gap-2 px-2.5 py-1.5 rounded-md text-left transition-colors opacity-60",
                        isSelected
                          ? "bg-zinc-800 text-zinc-400 opacity-100"
                          : "hover:bg-zinc-900 text-zinc-500"
                      )}
                    >
                      <span className="w-2 h-2 rounded-full shrink-0 bg-zinc-700" />
                      <span className="text-sm truncate">{peer.name}</span>
                      <span className="text-[10px] px-1.5 py-0.5 rounded bg-zinc-800/50 text-zinc-600 font-mono shrink-0">
                        {peer.circle}
                      </span>
                      {peer.last_seen && (
                        <span className="text-[10px] text-zinc-700 font-mono ml-auto shrink-0">
                          {timeAgo(peer.last_seen)}
                        </span>
                      )}
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </aside>
  );
}
