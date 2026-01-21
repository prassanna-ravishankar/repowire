"use client";

import React, { useState, useEffect, useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import {
  Activity,
  Users,
  MessageSquare,
  Settings,
  RefreshCw,
  Terminal,
  Shield,
  Wifi,
  WifiOff,
  Search,
  ChevronRight,
  ChevronDown,
  Clock,
  Folder,
  GitBranch,
  Monitor,
  X,
  Check,
  AlertCircle
} from "lucide-react";
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
  last_seen?: string;
  metadata?: {
    branch?: string;
    [key: string]: unknown;
  };
}

interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast" | "status_change";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error";
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

const API_BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [activeTab, setActiveTab] = useState<"conversations" | "network">("conversations");
  const [searchQuery, setSearchQuery] = useState("");
  const [selectedPeer, setSelectedPeer] = useState<Peer | null>(null);
  const [expandedConversations, setExpandedConversations] = useState<Set<string>>(new Set());

  // Fetch peers via REST (less frequent updates needed)
  const fetchPeers = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/peers`);
      if (res.ok) {
        const data = await res.json();
        setPeers(data.peers || data);
        setIsConnected(true);
      }
    } catch (error) {
      console.error("Failed to fetch peers:", error);
    }
  }, []);

  // Fetch initial events via REST
  const fetchEvents = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/events`);
      if (res.ok) {
        const data = await res.json();
        setEvents(data);
      }
    } catch (error) {
      console.error("Failed to fetch events:", error);
    }
  }, []);

  // Manual refresh
  const refreshData = useCallback(async () => {
    setIsRefreshing(true);
    await Promise.all([fetchPeers(), fetchEvents()]);
    setIsRefreshing(false);
  }, [fetchPeers, fetchEvents]);

  // SSE for real-time event streaming
  useEffect(() => {
    fetchPeers();
    fetchEvents();

    // Set up SSE connection for events
    const eventSource = new EventSource(`${API_BASE}/events/stream`);

    eventSource.onopen = () => {
      setIsConnected(true);
    };

    eventSource.onmessage = (e) => {
      try {
        const event = JSON.parse(e.data);
        setEvents(prev => {
          // Avoid duplicates by checking ID
          if (prev.some(existing => existing.id === event.id)) {
            return prev;
          }
          return [...prev, event];
        });
      } catch (error) {
        console.error("Failed to parse SSE event:", error);
      }
    };

    eventSource.onerror = () => {
      setIsConnected(false);
      // EventSource auto-reconnects, but we mark as disconnected
    };

    // Poll peers less frequently (every 10s)
    const peersInterval = setInterval(fetchPeers, 10000);

    return () => {
      eventSource.close();
      clearInterval(peersInterval);
    };
  }, [fetchPeers, fetchEvents]);

  // Group events into conversations (query + response pairs)
  const conversations: Conversation[] = React.useMemo(() => {
    const convos: Conversation[] = [];
    const queryEvents = events.filter(e => e.type === 'query');
    const responseEvents = events.filter(e => e.type === 'response');

    for (const query of queryEvents) {
      // Find matching response by query_id (reliable identifier)
      const response = responseEvents.find(r => r.query_id === query.id);

      convos.push({
        id: query.id,
        from: query.from || 'unknown',
        to: query.to || 'unknown',
        query,
        response,
        timestamp: query.timestamp,
        status: query.status === 'error' ? 'error' : response ? 'success' : 'pending'
      });
    }

    // Sort by timestamp descending (newest first)
    return convos.sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime());
  }, [events]);

  const filteredConversations = conversations.filter(c =>
    c.query.text?.toLowerCase().includes(searchQuery.toLowerCase()) ||
    c.from?.toLowerCase().includes(searchQuery.toLowerCase()) ||
    c.to?.toLowerCase().includes(searchQuery.toLowerCase()) ||
    c.response?.text?.toLowerCase().includes(searchQuery.toLowerCase())
  );

  const toggleConversation = (id: string) => {
    setExpandedConversations(prev => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const onlinePeers = peers.filter(p => p.status === 'online' || p.status === 'busy');
  const conversationCount = conversations.filter(c => c.status === 'success').length;

  return (
    <div className="min-h-screen bg-[#0a0a0a] text-zinc-400 font-sans selection:bg-blue-500/30">
      {/* Header */}
      <header className="border-b border-zinc-800/50 bg-[#0a0a0a]/80 backdrop-blur-md sticky top-0 z-50">
        <div className="max-w-7xl mx-auto px-4 h-16 flex items-center justify-between">
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-2">
              <img src="/logo-dark.webp" alt="Repowire" className="w-8 h-8 rounded-lg" />
              <span className="text-white font-bold tracking-tight text-lg">REPOWIRE</span>
              <span className="text-zinc-600 font-mono text-xs border border-zinc-800 px-1.5 py-0.5 rounded uppercase">Control Plane</span>
            </div>
          </div>

          <div className="flex items-center gap-4">
            <div className={cn(
              "flex items-center gap-2 px-3 py-1 rounded-full text-xs font-medium border transition-colors",
              isConnected
                ? "bg-emerald-500/10 border-emerald-500/20 text-emerald-500"
                : "bg-red-500/10 border-red-500/20 text-red-500"
            )}>
              {isConnected ? <Wifi className="w-3.5 h-3.5" /> : <WifiOff className="w-3.5 h-3.5" />}
              {isConnected ? "Connected" : "Disconnected"}
            </div>
            <button
              onClick={refreshData}
              className="p-2 hover:bg-zinc-800 rounded-lg transition-colors"
            >
              <RefreshCw className={cn("w-4 h-4", isRefreshing && "animate-spin")} />
            </button>
            <button className="p-2 hover:bg-zinc-800 rounded-lg transition-colors">
              <Settings className="w-4 h-4" />
            </button>
          </div>
        </div>
      </header>

      <main className="max-w-7xl mx-auto px-4 py-8">
        <div className="grid grid-cols-12 gap-8">
          {/* Sidebar */}
          <div className="col-span-12 lg:col-span-3 space-y-6">
            {/* Stats */}
            <div className="grid grid-cols-2 gap-3">
              <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl p-3">
                <div className="flex items-center gap-2 mb-1">
                  <Users className="w-3.5 h-3.5 text-zinc-500" />
                  <span className="text-[10px] font-bold uppercase text-zinc-500">Peers</span>
                </div>
                <div className="text-xl font-bold text-white">{onlinePeers.length}<span className="text-zinc-600 text-sm">/{peers.length}</span></div>
              </div>
              <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl p-3">
                <div className="flex items-center gap-2 mb-1">
                  <MessageSquare className="w-3.5 h-3.5 text-zinc-500" />
                  <span className="text-[10px] font-bold uppercase text-zinc-500">Convos</span>
                </div>
                <div className="text-xl font-bold text-white">{conversationCount}</div>
              </div>
            </div>

            {/* Peers List */}
            <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl overflow-hidden">
              <div className="px-4 py-3 border-b border-zinc-800/50 flex items-center justify-between">
                <h3 className="text-xs font-bold uppercase tracking-wider text-zinc-500">Peers</h3>
                <span className="flex h-2 w-2 rounded-full bg-emerald-500 animate-pulse" />
              </div>
              <div className="p-2 space-y-1 max-h-[300px] overflow-y-auto">
                {peers.map(peer => (
                  <button
                    key={peer.name}
                    onClick={() => setSelectedPeer(selectedPeer?.name === peer.name ? null : peer)}
                    className={cn(
                      "w-full flex items-center justify-between p-2 rounded-lg transition-colors text-left",
                      selectedPeer?.name === peer.name
                        ? "bg-blue-500/10 border border-blue-500/20"
                        : "hover:bg-zinc-800/50"
                    )}
                  >
                    <div className="flex items-center gap-3">
                      <div className={cn(
                        "w-2 h-2 rounded-full shrink-0",
                        peer.status === 'online' ? "bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.5)]" :
                        peer.status === 'busy' ? "bg-amber-500 shadow-[0_0_8px_rgba(245,158,11,0.5)]" :
                        "bg-zinc-700"
                      )} />
                      <span className="text-sm font-medium text-zinc-300 truncate">{peer.name}</span>
                    </div>
                    <ChevronRight className={cn(
                      "w-3.5 h-3.5 transition-transform",
                      selectedPeer?.name === peer.name && "rotate-90"
                    )} />
                  </button>
                ))}
                {peers.length === 0 && (
                  <div className="p-4 text-center text-xs text-zinc-600 italic">No peers registered</div>
                )}
              </div>
            </div>

            {/* Selected Peer Details */}
            {selectedPeer && (
              <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl overflow-hidden">
                <div className="px-4 py-3 border-b border-zinc-800/50 flex items-center justify-between">
                  <h3 className="text-xs font-bold uppercase tracking-wider text-zinc-500">Peer Details</h3>
                  <button onClick={() => setSelectedPeer(null)} className="p-1 hover:bg-zinc-800 rounded">
                    <X className="w-3 h-3" />
                  </button>
                </div>
                <div className="p-4 space-y-3">
                  <div className="flex items-center gap-2">
                    <div className={cn(
                      "w-3 h-3 rounded-full",
                      selectedPeer.status === 'online' ? "bg-emerald-500" :
                      selectedPeer.status === 'busy' ? "bg-amber-500" : "bg-zinc-700"
                    )} />
                    <span className="font-bold text-white">{selectedPeer.name}</span>
                    <span className={cn(
                      "text-[10px] uppercase px-1.5 py-0.5 rounded",
                      selectedPeer.status === 'online' ? "bg-emerald-500/10 text-emerald-500" :
                      selectedPeer.status === 'busy' ? "bg-amber-500/10 text-amber-500" :
                      "bg-zinc-800 text-zinc-500"
                    )}>{selectedPeer.status}</span>
                  </div>

                  <div className="space-y-2 text-xs">
                    <div className="flex items-start gap-2">
                      <Folder className="w-3.5 h-3.5 text-zinc-500 mt-0.5 shrink-0" />
                      <span className="text-zinc-400 break-all font-mono">{selectedPeer.path}</span>
                    </div>
                    {selectedPeer.metadata?.branch && (
                      <div className="flex items-center gap-2">
                        <GitBranch className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                        <span className="text-zinc-400 font-mono">{selectedPeer.metadata.branch}</span>
                      </div>
                    )}
                    {selectedPeer.tmux_session && (
                      <div className="flex items-center gap-2">
                        <Terminal className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                        <span className="text-zinc-400 font-mono">{selectedPeer.tmux_session}</span>
                      </div>
                    )}
                    <div className="flex items-center gap-2">
                      <Monitor className="w-3.5 h-3.5 text-zinc-500 shrink-0" />
                      <span className="text-zinc-400 font-mono">{selectedPeer.machine}</span>
                    </div>
                  </div>
                </div>
              </div>
            )}

            {/* Relay Mode */}
            <div className="bg-blue-600/5 border border-blue-500/10 rounded-xl p-4">
              <div className="flex items-center gap-2 mb-2 text-blue-400">
                <Shield className="w-4 h-4" />
                <span className="text-xs font-bold uppercase tracking-wider">Relay</span>
              </div>
              <p className="text-xs leading-relaxed text-zinc-500 mb-3">
                Local mode. Multi-machine mesh coming soon.
              </p>
              <input
                type="password"
                placeholder="API Key..."
                disabled
                className="w-full bg-black/40 border border-zinc-800 rounded-lg px-3 py-2 text-xs focus:outline-none mb-2 opacity-50 cursor-not-allowed"
              />
              <button
                disabled
                className="w-full py-2 bg-zinc-700 text-zinc-400 text-xs font-bold rounded-lg cursor-not-allowed"
              >
                Coming Soon
              </button>
            </div>
          </div>

          {/* Main Content */}
          <div className="col-span-12 lg:col-span-9 space-y-6">
            <div className="flex items-center justify-between gap-4">
              <div className="flex bg-zinc-900/50 border border-zinc-800/50 p-1 rounded-xl">
                <button
                  onClick={() => setActiveTab("conversations")}
                  className={cn(
                    "px-4 py-2 rounded-lg text-sm font-medium transition-all",
                    activeTab === "conversations" ? "bg-zinc-800 text-white shadow-sm" : "hover:text-zinc-300"
                  )}
                >
                  Conversations
                </button>
                <button
                  onClick={() => setActiveTab("network")}
                  className={cn(
                    "px-4 py-2 rounded-lg text-sm font-medium transition-all",
                    activeTab === "network" ? "bg-zinc-800 text-white shadow-sm" : "hover:text-zinc-300"
                  )}
                >
                  Network
                </button>
              </div>

              <div className="relative flex-1 max-w-sm">
                <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-zinc-600" />
                <input
                  type="text"
                  placeholder="Filter conversations..."
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  className="w-full bg-zinc-900/50 border border-zinc-800/50 rounded-xl py-2 pl-10 pr-4 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500/20 focus:border-blue-500/50"
                />
              </div>
            </div>

            <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-2xl overflow-hidden min-h-[600px]">
              {activeTab === "conversations" ? (
                <div className="p-4 space-y-3">
                  {filteredConversations.map((convo) => (
                    <ConversationCard
                      key={convo.id}
                      conversation={convo}
                      isExpanded={expandedConversations.has(convo.id)}
                      onToggle={() => toggleConversation(convo.id)}
                    />
                  ))}
                  {filteredConversations.length === 0 && (
                    <div className="h-[500px] flex flex-col items-center justify-center text-zinc-600 space-y-4">
                      <Terminal className="w-12 h-12 opacity-20" />
                      <p className="text-sm italic">No conversations yet...</p>
                      <p className="text-xs text-zinc-700">Ask a peer something to get started</p>
                    </div>
                  )}
                </div>
              ) : (
                <div className="h-[600px] flex flex-col items-center justify-center text-zinc-600 space-y-4">
                  <Activity className="w-12 h-12 opacity-20" />
                  <p className="text-sm font-medium">Network visualization coming soon</p>
                </div>
              )}
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}

function ConversationCard({
  conversation,
  isExpanded,
  onToggle
}: {
  conversation: Conversation;
  isExpanded: boolean;
  onToggle: () => void;
}) {
  const statusIcon = conversation.status === 'success'
    ? <Check className="w-3.5 h-3.5 text-emerald-500" />
    : conversation.status === 'pending'
    ? <RefreshCw className="w-3.5 h-3.5 text-blue-400 animate-spin" />
    : <AlertCircle className="w-3.5 h-3.5 text-red-400" />;

  return (
    <div className={cn(
      "border rounded-xl overflow-hidden transition-all",
      conversation.status === 'pending'
        ? "border-blue-500/20 bg-blue-500/[0.02]"
        : conversation.status === 'error'
        ? "border-red-500/20 bg-red-500/[0.02]"
        : "border-zinc-800/50 bg-zinc-800/10"
    )}>
      {/* Header - Always visible */}
      <button
        onClick={onToggle}
        className="w-full px-4 py-3 flex items-center gap-3 hover:bg-zinc-800/30 transition-colors"
      >
        <div className={cn(
          "transition-transform",
          isExpanded && "rotate-90"
        )}>
          <ChevronRight className="w-4 h-4 text-zinc-500" />
        </div>

        <div className="flex items-center gap-2 text-sm">
          <span className="font-bold text-zinc-300">@{conversation.from}</span>
          <ChevronRight className="w-3 h-3 text-zinc-600" />
          <span className="font-bold text-zinc-300">@{conversation.to}</span>
        </div>

        <div className="ml-auto flex items-center gap-3">
          {statusIcon}
          <span className="text-[10px] text-zinc-600 font-mono">
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
            <div className="text-[10px] uppercase text-blue-400 font-bold">Query</div>
            <div className="bg-[#050505] border border-zinc-800/50 rounded-lg p-3">
              <div className="text-sm text-zinc-300 prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-zinc-900 prose-pre:border prose-pre:border-zinc-700 prose-code:text-blue-300 prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5">
                <ReactMarkdown remarkPlugins={[remarkBreaks]}>{conversation.query.text}</ReactMarkdown>
              </div>
            </div>
          </div>

          {/* Response */}
          {conversation.response ? (
            <div className="space-y-1">
              <div className="text-[10px] uppercase text-emerald-400 font-bold">Response</div>
              <div className="bg-[#050505] border border-emerald-500/10 rounded-lg p-3">
                <div className={cn(
                  "text-sm prose prose-invert prose-sm max-w-none prose-p:my-1 prose-pre:bg-zinc-900 prose-pre:border prose-pre:border-zinc-700 prose-code:text-emerald-300 prose-ul:list-disc prose-ul:pl-4 prose-li:my-0.5",
                  conversation.status === 'error' ? "text-red-400" : "text-zinc-300"
                )}>
                  <ReactMarkdown remarkPlugins={[remarkBreaks]}>{conversation.response.text}</ReactMarkdown>
                </div>
              </div>
            </div>
          ) : conversation.status === 'pending' && (
            <div className="flex items-center gap-2 text-blue-400 text-xs">
              <RefreshCw className="w-3 h-3 animate-spin" />
              <span>Awaiting response...</span>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
