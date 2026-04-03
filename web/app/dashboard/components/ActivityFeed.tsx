"use client";

import { useState, useMemo } from "react";
import { Check, AlertCircle, RefreshCw, ChevronRight } from "lucide-react";
import { cn, timeAgo } from "../lib/utils";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Event, Peer } from "../types";

interface Conversation {
  id: string;
  from: string;
  to: string;
  query: Event;
  response?: Event;
  timestamp: string;
  status: "pending" | "success" | "error";
}

interface ActivityFeedProps {
  events: Event[];
  peerFilter?: string;
  peerName?: string;
  peers?: Peer[];
}

function ConversationCard({
  conversation,
  isExpanded,
  onToggle,
}: {
  conversation: Conversation;
  isExpanded: boolean;
  onToggle: () => void;
}) {
  const borderColor =
    conversation.status === "error"
      ? "border-error"
      : conversation.status === "pending"
      ? "border-primary-container"
      : "border-secondary-fixed";

  const statusIcon =
    conversation.status === "success" ? (
      <Check className="w-3.5 h-3.5 text-secondary" />
    ) : conversation.status === "pending" ? (
      <RefreshCw className="w-3.5 h-3.5 text-primary-container animate-spin" />
    ) : (
      <AlertCircle className="w-3.5 h-3.5 text-error" />
    );

  return (
    <div className="group transition-all duration-300">
      <div className={cn("bg-surface-container-low border-l-2 hover:bg-surface-container transition-colors", borderColor)}>
        <button
          onClick={onToggle}
          className="w-full px-4 py-3 flex items-center gap-3"
        >
          <div className={cn("transition-transform", isExpanded && "rotate-90")}>
            <ChevronRight className="w-4 h-4 text-outline" />
          </div>

          <div className="flex items-center gap-2 text-sm min-w-0">
            <span className="font-mono text-xs text-primary font-bold truncate">
              {conversation.from}
            </span>
            <span className="material-symbols-outlined text-[14px] text-outline">arrow_forward</span>
            <span className="font-mono text-xs text-on-surface-variant font-bold truncate">
              {conversation.to}
            </span>
          </div>

          <div className="ml-auto flex items-center gap-3 shrink-0">
            {statusIcon}
            <span className="text-[10px] text-outline font-mono tabular-nums">
              {timeAgo(conversation.timestamp)}
            </span>
          </div>
        </button>

        {!isExpanded && (
          <div className="px-4 pb-3 pl-11">
            <p className="text-sm text-on-surface-variant truncate font-mono">
              {conversation.query.text}
            </p>
          </div>
        )}

        {isExpanded && (
          <div className="px-4 pb-4 pl-11 space-y-3">
            <div className="space-y-1">
              <div className="text-[10px] uppercase text-primary font-bold tracking-widest">Query</div>
              <div className="bg-surface-container-lowest border border-outline-variant/10 p-3">
                <div className="text-sm text-on-surface prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-surface prose-pre:border prose-pre:border-outline-variant/20 prose-code:text-primary-fixed">
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>
                    {conversation.query.text}
                  </ReactMarkdown>
                </div>
              </div>
            </div>

            {conversation.response ? (
              <div className="space-y-1">
                <div className="text-[10px] uppercase text-secondary font-bold tracking-widest">Response</div>
                <div className="bg-surface-container-lowest border border-outline-variant/10 p-3">
                  <div
                    className={cn(
                      "text-sm prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-surface prose-pre:border prose-pre:border-outline-variant/20 prose-code:text-primary-fixed",
                      conversation.status === "error" ? "text-error" : "text-on-surface"
                    )}
                  >
                    <ReactMarkdown remarkPlugins={[remarkGfm]}>
                      {conversation.response.text}
                    </ReactMarkdown>
                  </div>
                </div>
              </div>
            ) : conversation.status === "pending" ? (
              <div className="flex items-center gap-2 text-primary-container text-xs">
                <RefreshCw className="w-3 h-3 animate-spin" />
                <span>Awaiting response...</span>
              </div>
            ) : null}
          </div>
        )}
      </div>
    </div>
  );
}

function NotificationRow({ event, isExpanded, onToggle }: { event: Event; isExpanded: boolean; onToggle: () => void }) {
  const isBroadcast = event.type === "broadcast";
  const borderColor = isBroadcast ? "border-secondary-fixed" : "border-tertiary-fixed-dim";

  return (
    <div className="group transition-all duration-300">
      <div className={cn("bg-surface-container-low border-l-2 hover:bg-surface-container transition-colors", borderColor)}>
        <button
          onClick={onToggle}
          className="w-full px-4 py-3 flex items-center gap-3"
        >
          <div className={cn("transition-transform", isExpanded && "rotate-90")}>
            <ChevronRight className="w-4 h-4 text-outline" />
          </div>
          <div className="flex items-center gap-2 text-sm min-w-0">
            <span className="font-mono text-xs font-bold text-on-surface truncate">
              {event.from || "?"}
            </span>
            {!isBroadcast && (
              <>
                <span className="material-symbols-outlined text-[14px] text-outline">arrow_forward</span>
                <span className="font-mono text-xs text-on-surface-variant truncate">
                  {event.to || "?"}
                </span>
              </>
            )}
            <span className="font-mono text-[10px] bg-surface-container-highest px-1.5 py-0.5 rounded text-on-surface-variant shrink-0">
              {isBroadcast ? "broadcast" : "notify"}
            </span>
          </div>
          <span className="ml-auto text-[10px] text-outline font-mono tabular-nums shrink-0">
            {timeAgo(event.timestamp)}
          </span>
        </button>
        {!isExpanded && (
          <div className="px-4 pb-3 pl-11">
            <p className="text-sm text-on-surface-variant truncate font-mono">{event.text}</p>
          </div>
        )}
        {isExpanded && (
          <div className="px-4 pb-4 pl-11">
            <div className="bg-surface-container-lowest border border-outline-variant/10 p-3">
              <div className="text-sm text-on-surface prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-surface prose-code:text-primary-fixed">
                <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

export function ActivityFeed({ events, peerFilter, peerName, peers }: ActivityFeedProps) {
  const { conversations, notifications } = useMemo(() => {
    const responseById = new Map<string, Event>();
    const queryEvents: Event[] = [];
    const notifEvents: Event[] = [];

    for (const e of events) {
      const matchesPeer = !peerFilter
        || e.from_peer_id === peerFilter || e.to_peer_id === peerFilter;

      if (e.type === "query") {
        if (matchesPeer) queryEvents.push(e);
      } else if (e.type === "response" && e.correlation_id) {
        responseById.set(e.correlation_id, e);
      } else if (e.type === "notification" || e.type === "broadcast") {
        if (matchesPeer) notifEvents.push(e);
      }
    }

    const conversations = queryEvents
      .map((query) => {
        const response = responseById.get(query.id);
        return {
          id: query.id,
          from: query.from || "unknown",
          to: query.to || "unknown",
          query,
          response,
          timestamp: query.timestamp,
          status: (query.status === "error" ? "error" : response ? "success" : "pending") as Conversation["status"],
        };
      });

    return { conversations, notifications: notifEvents };
  }, [events, peerFilter]);

  type ActivityItem =
    | { kind: "conversation"; data: Conversation }
    | { kind: "notification"; data: Event };

  const allActivity: ActivityItem[] = useMemo(() => {
    const items: ActivityItem[] = [
      ...conversations.map((c) => ({ kind: "conversation" as const, data: c })),
      ...notifications.map((n) => ({ kind: "notification" as const, data: n })),
    ];
    items.sort((a, b) => {
      const ta = "timestamp" in a.data ? a.data.timestamp : "";
      const tb = "timestamp" in b.data ? b.data.timestamp : "";
      return tb.localeCompare(ta);
    });
    return items;
  }, [conversations, notifications]);

  const [expandedItems, setExpandedItems] = useState<Set<string>>(new Set());

  const toggleItem = (id: string) => {
    setExpandedItems((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const isTopLevel = !peerFilter;
  const activePeerCount = useMemo(
    () => peers?.filter((p) => p.status === "online" || p.status === "busy").length ?? 0,
    [peers]
  );

  if (allActivity.length === 0) {
    return (
      <div className="text-center py-12 text-outline">
        <span className="material-symbols-outlined text-4xl mb-2">terminal</span>
        <p className="text-sm">
          {peerFilter ? `No activity involving ${peerName || peerFilter}` : "No activity yet"}
        </p>
        <p className="text-xs mt-1 text-outline-variant">Send a message to get started</p>
      </div>
    );
  }

  return (
    <div className="space-y-4 py-4">
      {/* Stats header for top-level Logs tab */}
      {isTopLevel && (
        <>
          <div className="grid grid-cols-2 gap-4 mb-6">
            <div className="bg-surface-container-low p-4 relative overflow-hidden">
              <div className="absolute top-0 left-0 w-full h-[2px] bg-primary-container" />
              <p className="text-[10px] font-bold uppercase tracking-widest text-outline font-headline">
                Total Events
              </p>
              <p className="text-2xl font-bold font-headline text-on-surface mt-1">{events.length}</p>
            </div>
            <div className="bg-surface-container-low p-4 relative overflow-hidden">
              <div className="absolute top-0 left-0 w-full h-[2px] bg-secondary-fixed" />
              <p className="text-[10px] font-bold uppercase tracking-widest text-outline font-headline">
                Active Peers
              </p>
              <div className="flex items-baseline gap-2 mt-1">
                <p className="text-2xl font-bold font-headline text-on-surface">{activePeerCount}</p>
                <span className="flex h-2 w-2 rounded-full bg-secondary-fixed animate-pulse" />
              </div>
            </div>
          </div>

          <div className="flex items-center justify-between mb-2">
            <h2 className="font-headline text-xs font-bold uppercase tracking-[0.2em] text-cyan-400">
              Mesh Core Activity
            </h2>
            <span className="text-[10px] font-medium text-outline bg-surface-container-highest px-2 py-0.5 rounded">
              LIVE_STREAM
            </span>
          </div>
        </>
      )}

      {/* Activity items */}
      <div className="space-y-4">
        {allActivity.map((item) =>
          item.kind === "conversation" ? (
            <ConversationCard
              key={item.data.id}
              conversation={item.data}
              isExpanded={expandedItems.has(item.data.id)}
              onToggle={() => toggleItem(item.data.id)}
            />
          ) : (
            <NotificationRow
              key={item.data.id}
              event={item.data}
              isExpanded={expandedItems.has(item.data.id)}
              onToggle={() => toggleItem(item.data.id)}
            />
          )
        )}
      </div>
    </div>
  );
}
