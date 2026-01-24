"use client";

import { useMemo } from "react";
import { Check, AlertCircle, RefreshCw, Ban } from "lucide-react";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
import ReactMarkdown from "react-markdown";
import remarkBreaks from "remark-breaks";

function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast" | "status_change";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error" | "blocked";
  peer?: string;
  new_status?: "online" | "busy" | "offline";
  query_id?: string;
}

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
  conversations: Conversation[];
}

type FeedItem = {
  id: string;
  timestamp: Date;
  type: "query" | "response" | "status_change";
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error" | "blocked";
  peer?: string;
  newStatus?: "online" | "busy" | "offline";
};

export function ActivityFeed({ events, conversations }: ActivityFeedProps) {
  const feedItems = useMemo(() => {
    const items: FeedItem[] = [];

    // Add conversations as query + response pairs
    for (const convo of conversations) {
      // Query
      items.push({
        id: `query-${convo.id}`,
        timestamp: new Date(convo.query.timestamp),
        type: "query",
        from: convo.from,
        to: convo.to,
        text: convo.query.text,
        status: convo.status === "error" ? "blocked" : "pending",
      });

      // Response (if exists)
      if (convo.response) {
        items.push({
          id: `response-${convo.id}`,
          timestamp: new Date(convo.response.timestamp),
          type: "response",
          from: convo.to,
          to: convo.from,
          text: convo.response.text,
          status: convo.status === "error" ? "error" : "success",
        });
      }
    }

    // Add status change events
    const statusEvents = events.filter((e) => e.type === "status_change");
    for (const event of statusEvents) {
      items.push({
        id: `status-${event.id}`,
        timestamp: new Date(event.timestamp),
        type: "status_change",
        peer: event.peer,
        newStatus: event.new_status,
        text: event.text,
      });
    }

    // Sort by timestamp descending (newest first)
    return items.sort((a, b) => b.timestamp.getTime() - a.timestamp.getTime());
  }, [events, conversations]);

  const formatTime = (date: Date) => {
    return date.toLocaleTimeString("en-US", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  };

  const truncateText = (text: string, maxLength: number = 80) => {
    if (text.length <= maxLength) return text;
    return text.slice(0, maxLength) + "...";
  };

  const StatusIcon = ({ status }: { status?: string }) => {
    switch (status) {
      case "success":
        return <Check className="w-3.5 h-3.5 text-emerald-500" />;
      case "error":
        return <AlertCircle className="w-3.5 h-3.5 text-red-400" />;
      case "blocked":
        return <Ban className="w-3.5 h-3.5 text-red-400" />;
      case "pending":
        return <RefreshCw className="w-3.5 h-3.5 text-amber-400 animate-spin" />;
      default:
        return null;
    }
  };

  if (feedItems.length === 0) {
    return (
      <div className="text-center py-12 text-zinc-600">
        <p className="text-sm">No activity yet</p>
        <p className="text-xs mt-1">Messages between peers will appear here</p>
      </div>
    );
  }

  return (
    <div className="space-y-1 font-mono text-sm">
      {feedItems.map((item) => (
        <div key={item.id} className="flex items-start gap-4 py-2 px-3 hover:bg-zinc-900/50 rounded">
          {/* Timestamp */}
          <span className="text-zinc-600 shrink-0 text-xs tabular-nums">
            {formatTime(item.timestamp)}
          </span>

          {/* Content */}
          <div className="flex-1 min-w-0">
            {item.type === "status_change" ? (
              <div className="flex items-center gap-2">
                <span className="text-zinc-400">{item.peer}</span>
                <span className="text-zinc-600">status</span>
                <span className="text-zinc-600">&rarr;</span>
                <span
                  className={cn(
                    item.newStatus === "online" && "text-emerald-500",
                    item.newStatus === "busy" && "text-amber-500",
                    item.newStatus === "offline" && "text-zinc-500"
                  )}
                >
                  {item.newStatus}
                </span>
              </div>
            ) : (
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <span className="text-zinc-300">{item.from}</span>
                  <span className="text-zinc-600">&rarr;</span>
                  <span className="text-zinc-300">{item.to}</span>
                  <div className="ml-auto flex items-center gap-2">
                    <StatusIcon status={item.status} />
                    {item.status === "blocked" && (
                      <span className="text-xs text-red-400">blocked</span>
                    )}
                    {item.status === "success" && (
                      <span className="text-xs text-emerald-500">success</span>
                    )}
                  </div>
                </div>
                <div className="text-zinc-500 text-xs pl-0">
                  &ldquo;{truncateText(item.text)}&rdquo;
                </div>
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}
