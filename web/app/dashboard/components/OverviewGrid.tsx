"use client";

import { useMemo, useState } from "react";
import { Activity, Plus, Radio, Users } from "lucide-react";
import { cn, statusDot, timeAgo } from "../lib/utils";
import { SpawnDialog } from "./SpawnDialog";
import type { Peer, Event } from "../types";

interface OverviewGridProps {
  peers: Peer[];
  events: Event[];
  apiBase: string;
  onSelectPeer: (peer: Peer) => void;
  onRefresh: () => void;
}

export function OverviewGrid({ peers, events, apiBase, onSelectPeer, onRefresh }: OverviewGridProps) {
  const [showSpawn, setShowSpawn] = useState(false);
  const activePeers = useMemo(
    () => peers.filter((p) => p.status === "online" || p.status === "busy"),
    [peers]
  );

  const busyCount = peers.filter((p) => p.status === "busy").length;

  const recentActivity = useMemo(() => {
    return events
      .filter((e) => e.type !== "status_change" && e.type !== "chat_turn")
      .sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
      .slice(0, 8);
  }, [events]);

  // Find last activity time for each peer (single-pass, O(N))
  const lastActivity = useMemo(() => {
    const map = new Map<string, Event>();
    for (const e of events) {
      const ts = new Date(e.timestamp).getTime();
      const update = (key: string) => {
        const prev = map.get(key);
        if (!prev || new Date(prev.timestamp).getTime() < ts) map.set(key, e);
      };
      if (e.from) update(e.from);
      if (e.to) update(e.to);
      if (e.peer) update(e.peer);
    }
    return map;
  }, [events]);

  return (
    <div className="flex-1 overflow-y-auto p-6">
      {/* Metrics row + spawn button */}
      <div className="flex gap-4 mb-6 items-center">
        <MetricCard
          icon={<Users className="w-4 h-4" />}
          label="Online"
          value={activePeers.length}
          color="text-emerald-500"
        />
        <MetricCard
          icon={<Radio className="w-4 h-4" />}
          label="Busy"
          value={busyCount}
          color="text-amber-500"
        />
        <MetricCard
          icon={<Activity className="w-4 h-4" />}
          label="Events"
          value={events.length}
          color="text-zinc-400"
        />
        <button
          onClick={() => setShowSpawn(true)}
          className="ml-auto flex items-center gap-2 px-4 py-2.5 rounded-lg border border-zinc-700 bg-zinc-900 hover:bg-zinc-800 text-zinc-300 text-sm font-medium transition-colors"
        >
          <Plus className="w-4 h-4" />
          New Session
        </button>
      </div>

      {showSpawn && (
        <SpawnDialog
          apiBase={apiBase}
          onClose={() => setShowSpawn(false)}
          onSpawned={onRefresh}
        />
      )}

      {/* Active peer cards */}
      {activePeers.length > 0 && (
        <div className="mb-8">
          <h3 className="text-[10px] font-mono text-zinc-600 uppercase tracking-wider mb-3">
            Active Peers
          </h3>
          <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-3">
            {activePeers.map((peer) => {
              const activity = lastActivity.get(peer.name);
              return (
                <button
                  key={peer.peer_id}
                  onClick={() => onSelectPeer(peer)}
                  className="bg-zinc-900/50 border border-zinc-800/50 rounded-lg p-4 text-left hover:border-zinc-700 hover:bg-zinc-900 transition-all group"
                >
                  {/* Name + status */}
                  <div className="flex items-center gap-2 mb-2">
                    <span className={cn("w-2 h-2 rounded-full", statusDot(peer.status))} />
                    <span className="text-sm font-medium text-zinc-200 group-hover:text-white transition-colors">
                      {peer.name}
                    </span>
                    <span
                      className={cn(
                        "text-[10px] px-1.5 py-0.5 rounded font-mono",
                        peer.status === "busy"
                          ? "bg-amber-500/10 text-amber-400"
                          : "bg-emerald-500/10 text-emerald-400"
                      )}
                    >
                      {peer.status}
                    </span>
                  </div>

                  {/* Details */}
                  <div className="space-y-1 text-xs text-zinc-500 font-mono">
                    <div className="flex items-center gap-2">
                      <span className="text-zinc-600">circle:</span>
                      <span>{peer.circle}</span>
                    </div>
                    {peer.metadata?.branch && (
                      <div className="flex items-center gap-2">
                        <span className="text-zinc-600">branch:</span>
                        <span className="truncate">{String(peer.metadata.branch)}</span>
                      </div>
                    )}
                    <div className="flex items-center gap-2">
                      <span className="text-zinc-600">path:</span>
                      <span className="truncate">{peer.path}</span>
                    </div>
                  </div>

                  {/* Last activity */}
                  {activity && (
                    <div className="mt-3 pt-2 border-t border-zinc-800/50 text-xs text-zinc-600 truncate">
                      {activity.type === "query" && `⇢ query → ${activity.to}`}
                      {activity.type === "response" && `⇠ response → ${activity.from}`}
                      {activity.type === "notification" && `⇢ notify → ${activity.to}`}
                      {activity.type === "broadcast" && `⇢ broadcast from ${activity.from}`}
                      <span className="ml-2 text-zinc-700">{timeAgo(activity.timestamp)}</span>
                    </div>
                  )}
                </button>
              );
            })}
          </div>
        </div>
      )}

      {/* No active peers fallback */}
      {activePeers.length === 0 && (
        <div className="text-center py-16">
          <div className="text-zinc-700 text-sm">No peers online</div>
          <div className="text-zinc-800 text-xs mt-1">
            Start an agent session to see it here
          </div>
        </div>
      )}

      {/* Recent mesh activity */}
      {recentActivity.length > 0 && (
        <div>
          <h3 className="text-[10px] font-mono text-zinc-600 uppercase tracking-wider mb-3">
            Recent Activity
          </h3>
          <div className="space-y-1">
            {recentActivity.map((event) => (
              <div
                key={event.id}
                className="flex items-center gap-3 px-3 py-2 rounded-md text-xs font-mono"
              >
                <span className="text-zinc-700 tabular-nums shrink-0">
                  {new Date(event.timestamp).toLocaleTimeString()}
                </span>
                <EventLabel event={event} />
                <span className="text-zinc-600 truncate">{event.text}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function MetricCard({
  icon,
  label,
  value,
  color,
}: {
  icon: React.ReactNode;
  label: string;
  value: number;
  color: string;
}) {
  return (
    <div className="flex items-center gap-3 bg-zinc-900/50 border border-zinc-800/50 rounded-lg px-4 py-3">
      <div className={color}>{icon}</div>
      <div>
        <div className="text-lg font-bold text-zinc-200 tabular-nums">{value}</div>
        <div className="text-[10px] text-zinc-600 uppercase tracking-wider">{label}</div>
      </div>
    </div>
  );
}

function EventLabel({ event }: { event: Event }) {
  switch (event.type) {
    case "query":
      return (
        <span className="text-blue-400 shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "response":
      return (
        <span className="text-emerald-400 shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "notification":
      return (
        <span className="text-purple-400 shrink-0">
          {event.from} → {event.to}
        </span>
      );
    case "broadcast":
      return (
        <span className="text-amber-400 shrink-0">
          {event.from} → all
        </span>
      );
    default:
      return <span className="text-zinc-500 shrink-0">{event.type}</span>;
  }
}
