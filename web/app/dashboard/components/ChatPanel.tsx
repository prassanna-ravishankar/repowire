"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ChevronRight } from "lucide-react";
import { cn } from "../lib/utils";
import { peerLabel } from "../types";
import type { Peer, Event } from "../types";

interface ChatPanelProps {
  peer: Peer;
  events: Event[];
}

function ToolCallBlock({ toolCalls }: { toolCalls: { name: string; input: string }[] }) {
  const [expanded, setExpanded] = useState(false);
  if (toolCalls.length === 0) return null;

  return (
    <div className="flex flex-col items-center w-full space-y-3 px-4 my-4">
      <div className="w-full flex items-center gap-4">
        <div className="h-[1px] flex-1 bg-outline-variant/30" />
        <button
          onClick={() => setExpanded(!expanded)}
          className="flex items-center gap-2 px-3 py-1 bg-surface-container-lowest border border-primary/20 rounded hover:border-primary/40 transition-colors"
        >
          <span className="material-symbols-outlined text-[14px] text-primary" style={{ fontVariationSettings: "'FILL' 1" }}>
            terminal
          </span>
          <span className="text-[9px] font-bold uppercase tracking-[0.15em] text-primary">
            {toolCalls.length} Tool Call{toolCalls.length > 1 ? "s" : ""}
          </span>
          <ChevronRight className={cn("w-3 h-3 text-primary transition-transform", expanded && "rotate-90")} />
        </button>
        <div className="h-[1px] flex-1 bg-outline-variant/30" />
      </div>

      {expanded && (
        <div className="w-full bg-surface-container-lowest p-4 border border-outline-variant/20 rounded-lg font-mono">
          {toolCalls.map((tc, i) => (
            <div key={i} className="flex flex-col gap-1 mb-2 last:mb-0">
              <div className="flex gap-2 text-xs">
                <span className="text-secondary-fixed">invoke</span>
                <span className="text-primary-fixed">{tc.name}</span>
              </div>
              <div className="pl-4 text-xs">
                <span className="text-outline">{tc.input}</span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function ChatPanel({ peer, events }: ChatPanelProps) {
  const bottomRef = useRef<HTMLDivElement>(null);

  const filtered = useMemo(() => {
    const id = peer.peer_id;
    return events
      .filter((e) => {
        if (e.type === "chat_turn") return e.peer_id === id;
        return e.from_peer_id === id || e.to_peer_id === id;
      })
      .sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }, [peer.peer_id, events]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [filtered.length]);

  if (filtered.length === 0) {
    return (
      <div className="flex items-center justify-center h-full text-outline text-sm">
        No activity for {peerLabel(peer)} yet
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3 p-4 mesh-bg overflow-y-auto h-full">
      {filtered.map((event) => {
        if (event.type === "chat_turn") {
          const isUser = event.role === "user";
          return (
            <div key={event.id}>
              <div className={cn("flex flex-col gap-1", isUser ? "items-start max-w-[85%]" : "items-end self-end max-w-[85%] ml-auto")}>
                <div className="flex items-center gap-2 mb-1">
                  {isUser && (
                    <span className="text-[10px] font-bold uppercase tracking-wider text-primary/60">
                      Operator
                    </span>
                  )}
                  <span className="text-[9px] text-outline-variant font-mono tabular-nums">
                    {new Date(event.timestamp).toLocaleTimeString()}
                  </span>
                  {!isUser && (
                    <span className="text-[10px] font-bold uppercase tracking-wider text-cyan-400">
                      {peerLabel(peer)}
                    </span>
                  )}
                </div>
                <div
                  className={cn(
                    "p-4 rounded-lg text-sm shadow-lg transition-all",
                    isUser
                      ? "bg-surface-container-low border-l-2 border-primary rounded-tl-none shadow-primary/5 group-hover:bg-surface-container"
                      : "bg-surface-container-highest border-r-2 border-cyan-400 rounded-tr-none shadow-cyan-400/5"
                  )}
                >
                  {isUser ? (
                    <p className="whitespace-pre-wrap leading-relaxed text-on-surface">{event.text}</p>
                  ) : (
                    <div className="prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-surface-container-lowest prose-pre:border prose-pre:border-outline-variant/20 prose-code:text-primary-fixed prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5 prose-table:border-collapse prose-th:border prose-th:border-outline-variant/30 prose-th:px-3 prose-th:py-1.5 prose-th:bg-surface-container-low prose-td:border prose-td:border-outline-variant/30 prose-td:px-3 prose-td:py-1.5">
                      <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
                    </div>
                  )}
                </div>
              </div>
              {!isUser && event.tool_calls && event.tool_calls.length > 0 && (
                <ToolCallBlock toolCalls={event.tool_calls} />
              )}
            </div>
          );
        }

        // Repowire trace row
        const label =
          event.type === "query"
            ? `query ${event.from} → ${event.to}`
            : event.type === "response"
            ? `response ${event.from} → ${event.to}`
            : event.type === "notification"
            ? `notify ${event.from} → ${event.to}`
            : `broadcast from ${event.from}`;

        return (
          <div key={event.id} className="flex items-start gap-2 text-xs font-mono text-outline px-2">
            <span className="shrink-0 text-outline-variant tabular-nums">
              {new Date(event.timestamp).toLocaleTimeString()}
            </span>
            <span className="text-on-surface-variant">{label}</span>
            <span className="truncate text-outline">{event.text}</span>
          </div>
        );
      })}
      <div ref={bottomRef} />
    </div>
  );
}
