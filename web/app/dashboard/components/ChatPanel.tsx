"use client";

import { useEffect, useMemo, useRef } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { cn } from "../lib/utils";
import type { Peer, Event } from "../types";

interface ChatPanelProps {
  peer: Peer;
  events: Event[];
}

export function ChatPanel({ peer, events }: ChatPanelProps) {
  const bottomRef = useRef<HTMLDivElement>(null);

  const filtered = useMemo(() => {
    return events
      .filter((e) => {
        if (e.type === "chat_turn") return e.peer === peer.name;
        if (e.type === "query" || e.type === "response" || e.type === "notification" || e.type === "broadcast") {
          return e.from === peer.name || e.to === peer.name;
        }
        return false;
      })
      .sort((a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime());
  }, [peer.name, events]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [filtered.length]);

  if (filtered.length === 0) {
    return (
      <div className="flex items-center justify-center h-full text-zinc-600 text-sm">
        No activity for {peer.name} yet
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3 p-4 overflow-y-auto h-full">
      {filtered.map((event) => {
        if (event.type === "chat_turn") {
          const isUser = event.role === "user";
          return (
            <div key={event.id} className={cn("flex flex-col gap-1", isUser ? "items-end" : "items-start")}>
              <span className="text-[10px] text-zinc-500 font-mono px-1">
                {isUser ? "user" : peer.name}
              </span>
              <div
                className={cn(
                  "max-w-[80%] rounded-xl px-4 py-3 text-sm",
                  isUser
                    ? "bg-zinc-700 text-zinc-200"
                    : "bg-zinc-800/50 text-zinc-300"
                )}
              >
                {isUser ? (
                  <p className="whitespace-pre-wrap">{event.text}</p>
                ) : (
                  <div className="prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-zinc-900 prose-pre:border prose-pre:border-zinc-700 prose-code:text-emerald-300 prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5 prose-table:border-collapse prose-th:border prose-th:border-zinc-700 prose-th:px-3 prose-th:py-1.5 prose-th:bg-zinc-900 prose-td:border prose-td:border-zinc-700 prose-td:px-3 prose-td:py-1.5">
                    <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
                  </div>
                )}
              </div>
              <span className="text-[10px] text-zinc-600 font-mono tabular-nums px-1">
                {new Date(event.timestamp).toLocaleTimeString()}
              </span>
            </div>
          );
        }

        // Repowire trace row
        const label =
          event.type === "query"
            ? `⇢ query ${event.from} → ${event.to}`
            : event.type === "response"
            ? `⇢ response ${event.from} → ${event.to}`
            : event.type === "notification"
            ? `⇢ notify ${event.from} → ${event.to}`
            : `⇢ broadcast from ${event.from}`;

        return (
          <div key={event.id} className="flex items-start gap-2 text-xs font-mono text-zinc-600">
            <span className="shrink-0 text-zinc-700">{new Date(event.timestamp).toLocaleTimeString()}</span>
            <span className="text-zinc-500">{label}</span>
            <span className="truncate text-zinc-600">{event.text}</span>
          </div>
        );
      })}
      <div ref={bottomRef} />
    </div>
  );
}
