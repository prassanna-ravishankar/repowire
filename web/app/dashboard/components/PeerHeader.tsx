"use client";

import { X, Folder, GitBranch, Monitor, Terminal } from "lucide-react";
import { cn, statusDot } from "../lib/utils";
import type { Peer } from "../types";

interface PeerHeaderProps {
  peer: Peer;
  onClose: () => void;
}

export function PeerHeader({ peer, onClose }: PeerHeaderProps) {
  return (
    <div className="flex items-center gap-4 px-4 py-3 border-b border-zinc-800 bg-zinc-950 shrink-0">
      {/* Name + status */}
      <div className="flex items-center gap-2.5">
        <span className={cn("w-2.5 h-2.5 rounded-full", statusDot(peer.status))} />
        <span className="text-base font-semibold text-zinc-200">{peer.name}</span>
        <span
          className={cn(
            "text-[10px] px-2 py-0.5 rounded-full font-medium",
            peer.status === "online" && "bg-emerald-500/10 text-emerald-400",
            peer.status === "busy" && "bg-amber-500/10 text-amber-400",
            peer.status === "offline" && "bg-zinc-700/50 text-zinc-500"
          )}
        >
          {peer.status}
        </span>
      </div>

      {/* Metadata chips */}
      <div className="flex items-center gap-3 text-xs text-zinc-500 font-mono overflow-hidden">
        <div className="flex items-center gap-1 shrink-0">
          <span className="text-zinc-600">circle:</span>
          <span>{peer.circle}</span>
        </div>
        {peer.metadata?.branch && (
          <div className="flex items-center gap-1 shrink-0">
            <GitBranch className="w-3 h-3 text-zinc-600" />
            <span>{String(peer.metadata.branch)}</span>
          </div>
        )}
        {peer.path && (
          <div className="flex items-center gap-1 min-w-0">
            <Folder className="w-3 h-3 text-zinc-600 shrink-0" />
            <span className="truncate">{peer.path}</span>
          </div>
        )}
        {peer.machine && (
          <div className="flex items-center gap-1 shrink-0 hidden lg:flex">
            <Monitor className="w-3 h-3 text-zinc-600" />
            <span>{peer.machine}</span>
          </div>
        )}
        {peer.tmux_session && (
          <div className="flex items-center gap-1 shrink-0 hidden xl:flex">
            <Terminal className="w-3 h-3 text-zinc-600" />
            <span>{peer.tmux_session}</span>
          </div>
        )}
      </div>

      {/* Close button */}
      <button
        onClick={onClose}
        className="ml-auto p-1.5 hover:bg-zinc-800 rounded-md transition-colors shrink-0"
      >
        <X className="w-4 h-4 text-zinc-500" />
      </button>
    </div>
  );
}
