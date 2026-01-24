"use client";

import { useState } from "react";
import { PeerCard } from "./PeerCard";

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

interface CircleGroupProps {
  name: string;
  peers: Peer[];
}

export function CircleGroup({ name, peers }: CircleGroupProps) {
  const [expandedPeer, setExpandedPeer] = useState<string | null>(null);

  const togglePeer = (peerName: string) => {
    setExpandedPeer(expandedPeer === peerName ? null : peerName);
  };

  return (
    <div className="border border-zinc-800 rounded-lg overflow-hidden min-w-[240px] max-w-[320px] flex-shrink-0">
      {/* Circle header */}
      <div className="px-3 py-2 bg-zinc-900/50 border-b border-zinc-800">
        <span className="text-[10px] font-mono text-zinc-500 uppercase tracking-wider">{name}</span>
      </div>

      {/* Peer cards - stacked vertically */}
      <div className="p-2 space-y-2">
        {peers.map((peer) => (
          <PeerCard
            key={peer.name}
            peer={peer}
            expanded={expandedPeer === peer.name}
            onToggle={() => togglePeer(peer.name)}
          />
        ))}
      </div>
    </div>
  );
}
