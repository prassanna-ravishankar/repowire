"use client";

import { useState } from "react";
import { Check, AlertCircle, RefreshCw, ChevronRight } from "lucide-react";
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

function ConversationCard({
  conversation,
  isExpanded,
  onToggle,
}: {
  conversation: Conversation;
  isExpanded: boolean;
  onToggle: () => void;
}) {
  const statusIcon =
    conversation.status === "success" ? (
      <Check className="w-3.5 h-3.5 text-emerald-500" />
    ) : conversation.status === "pending" ? (
      <RefreshCw className="w-3.5 h-3.5 text-blue-400 animate-spin" />
    ) : (
      <AlertCircle className="w-3.5 h-3.5 text-red-400" />
    );

  return (
    <div
      className={cn(
        "border rounded-lg overflow-hidden transition-all",
        conversation.status === "pending"
          ? "border-blue-500/20 bg-blue-500/[0.02]"
          : conversation.status === "error"
          ? "border-red-500/20 bg-red-500/[0.02]"
          : "border-zinc-800/50 bg-zinc-800/10"
      )}
    >
      {/* Header - Always visible */}
      <button
        onClick={onToggle}
        className="w-full px-4 py-3 flex items-center gap-3 hover:bg-zinc-800/30 transition-colors"
      >
        <div className={cn("transition-transform", isExpanded && "rotate-90")}>
          <ChevronRight className="w-4 h-4 text-zinc-500" />
        </div>

        <div className="flex items-center gap-2 text-sm">
          <span className="font-medium text-zinc-300">@{conversation.from}</span>
          <ChevronRight className="w-3 h-3 text-zinc-600" />
          <span className="font-medium text-zinc-300">@{conversation.to}</span>
        </div>

        <div className="ml-auto flex items-center gap-3">
          {statusIcon}
          <span className="text-[10px] text-zinc-600 font-mono tabular-nums">
            {new Date(conversation.timestamp).toLocaleTimeString()}
          </span>
        </div>
      </button>

      {/* Collapsed preview */}
      {!isExpanded && (
        <div className="px-4 pb-3 pl-11">
          <p className="text-sm text-zinc-500 truncate">
            Q: {conversation.query.text}
          </p>
        </div>
      )}

      {/* Expanded content */}
      {isExpanded && (
        <div className="px-4 pb-4 pl-11 space-y-3">
          {/* Query */}
          <div className="space-y-1">
            <div className="text-[10px] uppercase text-blue-400 font-bold">
              Query
            </div>
            <div className="bg-zinc-950 border border-zinc-800/50 rounded-lg p-3">
              <div className="text-sm text-zinc-300 prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-zinc-900 prose-pre:border prose-pre:border-zinc-700 prose-code:text-blue-300 prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5">
                <ReactMarkdown remarkPlugins={[remarkBreaks]}>
                  {conversation.query.text}
                </ReactMarkdown>
              </div>
            </div>
          </div>

          {/* Response */}
          {conversation.response ? (
            <div className="space-y-1">
              <div className="text-[10px] uppercase text-emerald-400 font-bold">
                Response
              </div>
              <div className="bg-zinc-950 border border-emerald-500/10 rounded-lg p-3">
                <div
                  className={cn(
                    "text-sm prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-zinc-900 prose-pre:border prose-pre:border-zinc-700 prose-code:text-emerald-300 prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5",
                    conversation.status === "error"
                      ? "text-red-400"
                      : "text-zinc-300"
                  )}
                >
                  <ReactMarkdown remarkPlugins={[remarkBreaks]}>
                    {conversation.response.text}
                  </ReactMarkdown>
                </div>
              </div>
            </div>
          ) : conversation.status === "pending" ? (
            <div className="flex items-center gap-2 text-blue-400 text-xs">
              <RefreshCw className="w-3 h-3 animate-spin" />
              <span>Awaiting response...</span>
            </div>
          ) : null}
        </div>
      )}
    </div>
  );
}

export function ActivityFeed({ conversations }: ActivityFeedProps) {
  const [expandedConversations, setExpandedConversations] = useState<Set<string>>(
    new Set()
  );

  const toggleConversation = (id: string) => {
    setExpandedConversations((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  if (conversations.length === 0) {
    return (
      <div className="text-center py-12 text-zinc-600">
        <p className="text-sm">No conversations yet</p>
        <p className="text-xs mt-1">Ask a peer something to get started</p>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {conversations.map((convo) => (
        <ConversationCard
          key={convo.id}
          conversation={convo}
          isExpanded={expandedConversations.has(convo.id)}
          onToggle={() => toggleConversation(convo.id)}
        />
      ))}
    </div>
  );
}
