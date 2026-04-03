"use client";

import { useMemo } from "react";
import { cn, statusDot, statusBorderColor, statusTopStrip, statusTextColor, shortPath } from "../lib/utils";
import { RoleBadge } from "./RoleBadge";
import type { Peer, Event } from "../types";
import { peerLabel } from "../types";

interface OverviewGridProps {
  peers: Peer[];
  events: Event[];
  onSelectPeer: (peer: Peer) => void;
  circleFilter?: string | null;
}

export function OverviewGrid({ peers, events, onSelectPeer, circleFilter }: OverviewGridProps) {

  // Filter out service peers and apply circle filter
  const gridPeers = useMemo(
    () => peers.filter((p) => p.role !== "service" && (!circleFilter || p.circle === circleFilter)),
    [peers, circleFilter]
  );

  const activePeers = useMemo(
    () => gridPeers.filter((p) => p.status === "online" || p.status === "busy"),
    [gridPeers]
  );

  const offlinePeers = useMemo(
    () => gridPeers.filter((p) => p.status === "offline"),
    [gridPeers]
  );

  const busyCount = useMemo(
    () => gridPeers.filter((p) => p.status === "busy").length,
    [gridPeers]
  );

  const recentActivity = useMemo(() => {
    return events
      .filter((e) => e.type !== "status_change" && e.type !== "chat_turn")
      .sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
      .slice(0, 8);
  }, [events]);

  return (
    <div className="px-6 max-w-2xl md:max-w-5xl mx-auto py-4">
      {/* Section Header */}
      <section className="mb-10">
        <h2 className="font-headline text-3xl font-bold text-primary mb-2 tracking-tight">
          Active Peer Grid
        </h2>
        <p className="text-sm font-light text-on-surface-variant">
          Orchestrating {activePeers.length} active instance{activePeers.length !== 1 ? "s" : ""}.
        </p>
      </section>

      {/* Active Peer Cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4 mb-8">
        {activePeers.map((peer) => (
          <PeerCard key={peer.peer_id} peer={peer} onSelect={onSelectPeer} />
        ))}
      </div>

      {/* Offline Peers */}
      {offlinePeers.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4 mb-8">
          {offlinePeers.map((peer) => (
            <PeerCard key={peer.peer_id} peer={peer} onSelect={onSelectPeer} offline />
          ))}
        </div>
      )}

      {/* No peers fallback */}
      {peers.length === 0 && (
        <div className="text-center py-16">
          <span className="material-symbols-outlined text-4xl text-outline mb-2">hub</span>
          <p className="text-sm text-outline">No peers registered</p>
          <p className="text-xs text-outline-variant mt-1">Start an agent session to see it here</p>
        </div>
      )}

      {/* Stats Bar */}
      <div className="flex gap-2 mb-8">
        <div className="flex-1 bg-surface-container-lowest p-4 rounded border border-outline-variant/10">
          <span className="text-[10px] uppercase font-headline font-bold text-primary/40 block mb-1">
            Online
          </span>
          <span className="text-xl font-headline font-bold text-primary">
            {activePeers.length}
          </span>
        </div>
        <div className="flex-1 bg-surface-container-lowest p-4 rounded border border-outline-variant/10">
          <span className="text-[10px] uppercase font-headline font-bold text-tertiary-fixed-dim/40 block mb-1">
            Busy
          </span>
          <span className="text-xl font-headline font-bold text-tertiary-fixed-dim">
            {busyCount}
          </span>
        </div>
        <div className="flex-1 bg-surface-container-lowest p-4 rounded border border-outline-variant/10">
          <span className="text-[10px] uppercase font-headline font-bold text-on-surface-variant/40 block mb-1">
            Events
          </span>
          <span className="text-xl font-headline font-bold text-on-surface-variant">
            {events.length}
          </span>
        </div>
      </div>

      {/* Recent Activity */}
      {recentActivity.length > 0 && (
        <section>
          <h3 className="font-headline text-xs font-bold uppercase tracking-[0.2em] text-primary/60 mb-3">
            Recent Activity
          </h3>
          <div className="space-y-1">
            {recentActivity.map((event) => (
              <div
                key={event.id}
                className="flex items-center gap-3 px-3 py-2 text-xs font-mono"
              >
                <span className="text-outline tabular-nums shrink-0">
                  {new Date(event.timestamp).toLocaleTimeString()}
                </span>
                <EventLabel event={event} />
                <span className="text-on-surface-variant truncate">{event.text}</span>
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}

function PeerCard({
  peer,
  onSelect,
  offline,
}: {
  peer: Peer;
  onSelect: (peer: Peer) => void;
  offline?: boolean;
}) {
  return (
    <button
      onClick={() => onSelect(peer)}
      className={cn(
        "relative group text-left transition-all duration-300",
        offline && "opacity-60 grayscale-[0.5]",
        !offline && "md:hover:translate-y-[-4px]"
      )}
    >
      <div className={cn("absolute -top-[1px] left-0 w-full h-[2px]", statusTopStrip(peer.status))} />
      <div
        className={cn(
          "bg-surface-container-low p-5 border-l-4 transition-all duration-300 overflow-hidden",
          statusBorderColor(peer.status),
          !offline && "hover:bg-surface-container-high"
        )}
      >
        {/* Name + Status */}
        <div className="flex justify-between items-start mb-4">
          <div className="flex flex-col">
            <span className="font-headline text-lg font-bold text-on-surface tracking-wide truncate">
              {peerLabel(peer)}
            </span>
            <div className="flex items-center gap-2 mt-1">
              <span className={cn("w-2 h-2 rounded-full", statusDot(peer.status))} />
              <span className={cn("text-[10px] uppercase font-bold tracking-widest", statusTextColor(peer.status))}>
                {peer.status}
              </span>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <RoleBadge role={peer.role} />
            {peer.circle && (
              <span className="font-mono text-[10px] bg-surface-container-highest px-2 py-1 text-on-surface-variant uppercase">
                {peer.circle}
              </span>
            )}
          </div>
        </div>

        {/* Description */}
        {peer.description && (
          <p className="text-sm text-on-surface mb-6 leading-relaxed">
            <span className={cn("font-mono mr-1", statusTextColor(peer.status) + "/60")}>&gt;</span>
            {peer.description}
          </p>
        )}

        {/* Metadata */}
        <div className="flex items-center gap-3 text-on-surface-variant min-w-0">
          {peer.path && <PeerPath path={peer.path} />}
          {peer.metadata?.branch && (
            <div className="flex items-center gap-1.5 shrink-0">
              <span className="material-symbols-outlined text-sm">commit</span>
              <span className="text-[11px] font-mono">{String(peer.metadata.branch)}</span>
            </div>
          )}
        </div>
      </div>
    </button>
  );
}

function PeerPath({ path }: { path: string }) {
  const { folder, parent } = shortPath(path);
  return (
    <div className="flex items-center gap-1.5 min-w-0">
      <span className="material-symbols-outlined text-sm shrink-0">folder_open</span>
      <span className="text-[11px] font-mono text-outline truncate">{parent}</span>
      <span className="text-[11px] font-mono shrink-0">{folder}</span>
    </div>
  );
}

function EventLabel({ event }: { event: Event }) {
  switch (event.type) {
    case "query":
      return (
        <span className="text-primary shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "response":
      return (
        <span className="text-secondary shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "notification":
      return (
        <span className="text-tertiary shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "broadcast":
      return (
        <span className="text-secondary-fixed shrink-0">
          {event.from} → all
        </span>
      );
    default:
      return <span className="text-outline shrink-0">{event.type}</span>;
  }
}
