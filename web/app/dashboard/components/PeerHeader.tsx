"use client";

import { useState } from "react";
import { ArrowLeft, Copy, Check } from "lucide-react";
import { cn, statusDot, statusTextColor, shortPath } from "../lib/utils";
import { RoleBadge } from "./RoleBadge";
import { peerLabel } from "../types";
import type { Peer } from "../types";

interface PeerHeaderProps {
  peer: Peer;
  onClose: () => void;
}

export function PeerHeader({ peer, onClose }: PeerHeaderProps) {
  const [copied, setCopied] = useState(false);

  const copyName = () => {
    navigator.clipboard.writeText(peer.name);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="shrink-0">
      <div className="flex items-center gap-4 px-4 py-3">
        {/* Back button */}
        <button
          onClick={onClose}
          className="flex items-center justify-center w-10 h-10 rounded-lg hover:bg-cyan-400/10 transition-colors active:scale-95 duration-200"
        >
          <ArrowLeft className="w-5 h-5 text-cyan-400" />
        </button>

        {/* Name + status */}
        <div className="flex flex-col min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h1 className="font-headline text-xl font-bold tracking-widest text-cyan-400 uppercase truncate">
              {peerLabel(peer)}
            </h1>
            <button
              onClick={copyName}
              className="flex items-center gap-1 text-[10px] text-outline font-mono hover:text-on-surface-variant transition-colors shrink-0"
              title="Copy peer name"
            >
              {copied ? <Check className="w-3 h-3 text-secondary" /> : <Copy className="w-3 h-3" />}
            </button>
          </div>
          <div className="flex items-center gap-1.5">
            <span className={cn("w-1.5 h-1.5 rounded-full", statusDot(peer.status))} />
            <span className="text-[10px] font-body uppercase tracking-widest text-on-surface-variant">
              {peer.status === "offline" ? "offline" : "active session"}
            </span>
          </div>
        </div>

        {/* Metadata */}
        <div className="hidden sm:flex items-center gap-3 text-xs text-on-surface-variant font-mono">
          <RoleBadge role={peer.role} />
          {peer.backend && (
            <span className="bg-surface-container-highest px-2 py-1 text-[10px] uppercase">
              {peer.backend}
            </span>
          )}
          <span className="bg-surface-container-highest px-2 py-1 text-[10px] uppercase">
            {peer.circle}
          </span>
          {peer.metadata?.branch && (
            <span className={cn("text-[10px]", statusTextColor(peer.status))}>
              {String(peer.metadata.branch)}
            </span>
          )}
          {peer.path && (() => {
            const { folder, parent } = shortPath(peer.path);
            return (
              <span className="text-[10px] text-outline truncate max-w-[12rem] hidden md:inline">
                {parent}<span className="text-on-surface-variant">{folder}</span>
              </span>
            );
          })()}
          {peer.machine && (
            <span className="text-[10px] text-outline hidden lg:inline">
              {peer.machine}
            </span>
          )}
        </div>
      </div>

      {/* Description */}
      {peer.description && (
        <div className="px-4 pb-2">
          <p className="text-xs text-on-surface-variant font-mono truncate">
            <span className="text-primary/60 mr-1">&gt;</span>
            {peer.description}
          </p>
        </div>
      )}

      {/* Separator */}
      <div className="bg-surface-container-low h-[2px] w-full" />
    </div>
  );
}
