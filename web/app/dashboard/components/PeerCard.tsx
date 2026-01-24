"use client";

import { ChevronDown, Folder, GitBranch, Terminal, Monitor, Clock } from "lucide-react";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

interface Peer {
  name: string;
  status: "online" | "busy" | "offline";
  machine: string;
  path: string;
  tmux_session?: string;
  circle: string;
  last_seen?: string;
  metadata?: {
    branch?: string;
    [key: string]: unknown;
  };
}

interface PeerCardProps {
  peer: Peer;
  expanded: boolean;
  onToggle: () => void;
}

export function PeerCard({ peer, expanded, onToggle }: PeerCardProps) {
  const statusColor = peer.status === "online"
    ? "bg-emerald-500"
    : peer.status === "busy"
    ? "bg-amber-500"
    : "bg-zinc-600";

  const formatLastSeen = (lastSeen?: string) => {
    if (!lastSeen) return null;
    const date = new Date(lastSeen);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    const diffMins = Math.floor(diffMs / 60000);

    if (diffMins < 1) return "just now";
    if (diffMins < 60) return `${diffMins} min ago`;
    const diffHours = Math.floor(diffMins / 60);
    if (diffHours < 24) return `${diffHours}h ago`;
    return date.toLocaleDateString();
  };

  return (
    <div
      className={cn(
        "bg-zinc-900 rounded-md transition-all cursor-pointer",
        expanded ? "ring-1 ring-zinc-700" : "hover:bg-zinc-800/80"
      )}
      onClick={onToggle}
    >
      {/* Compact view - always visible */}
      <div className="p-3 flex items-center gap-3">
        <div className={cn("w-2 h-2 rounded-full shrink-0", statusColor)} />
        <span className="text-sm font-medium text-zinc-200 truncate">{peer.name}</span>
        {peer.metadata?.branch && (
          <span className="text-xs text-zinc-500 font-mono truncate">{peer.metadata.branch}</span>
        )}
        <div className="ml-auto">
          <ChevronDown
            className={cn(
              "w-4 h-4 text-zinc-500 transition-transform",
              expanded && "rotate-180"
            )}
          />
        </div>
      </div>

      {/* Expanded view */}
      {expanded && (
        <div className="px-3 pb-3 pt-0 border-t border-zinc-800 mt-0">
          <div className="pt-3 space-y-2 text-xs font-mono text-zinc-400">
            <div className="flex items-start gap-2">
              <Folder className="w-3.5 h-3.5 text-zinc-500 mt-0.5 shrink-0" />
              <span className="break-all">{peer.path}</span>
            </div>
            <div className="flex items-center gap-2">
              <Monitor className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
              <span>{peer.machine}</span>
            </div>
            {peer.tmux_session && (
              <div className="flex items-center gap-2">
                <Terminal className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                <span>{peer.tmux_session}</span>
              </div>
            )}
            {peer.metadata?.branch && (
              <div className="flex items-center gap-2">
                <GitBranch className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                <span>{peer.metadata.branch}</span>
              </div>
            )}
            {peer.last_seen && (
              <div className="flex items-center gap-2">
                <Clock className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                <span>{formatLastSeen(peer.last_seen)}</span>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
