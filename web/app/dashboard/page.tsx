"use client";

import React, { useState, useEffect, useCallback, useMemo } from "react";
import { RefreshCw, Wifi, WifiOff } from "lucide-react";
import { ActivityFeed } from "./components/ActivityFeed";
import { ChatPanel } from "./components/ChatPanel";
import { ComposeBar } from "./components/ComposeBar";
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

interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast" | "status_change" | "chat_turn";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error";
  peer?: string;
  role?: "user" | "assistant";
  new_status?: "online" | "busy" | "offline";
  query_id?: string;
  correlation_id?: string;
}

const API_BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8377";

const STATUS_ORDER = { online: 0, busy: 1, offline: 2 };

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeer, setSelectedPeer] = useState<Peer | null>(null);
  const [activeTab, setActiveTab] = useState<"chat" | "trace">("chat");

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

  const refreshData = useCallback(async () => {
    setIsRefreshing(true);
    await Promise.all([fetchPeers(), fetchEvents()]);
    setIsRefreshing(false);
  }, [fetchPeers, fetchEvents]);

  useEffect(() => {
    fetchPeers();
    fetchEvents();

    const eventSource = new EventSource(`${API_BASE}/events/stream`);

    eventSource.onopen = () => setIsConnected(true);

    eventSource.onmessage = (e) => {
      try {
        const event = JSON.parse(e.data) as Event;
        setEvents((prev) => {
          if (prev.some((existing) => existing.id === event.id)) return prev;
          return [...prev, event];
        });
      } catch (error) {
        console.error("Failed to parse SSE event:", error);
      }
    };

    eventSource.onerror = () => setIsConnected(false);

    const peersInterval = setInterval(fetchPeers, 10000);

    return () => {
      eventSource.close();
      clearInterval(peersInterval);
    };
  }, [fetchPeers, fetchEvents]);

  const sortedPeers = useMemo(
    () => [...peers].sort((a, b) => STATUS_ORDER[a.status] - STATUS_ORDER[b.status]),
    [peers]
  );

  const onlineCount = peers.filter((p) => p.status === "online" || p.status === "busy").length;

  // Keep selectedPeer in sync with updated peer data
  const currentSelectedPeer = useMemo(
    () => selectedPeer ? (peers.find((p) => p.name === selectedPeer.name) ?? selectedPeer) : null,
    [peers, selectedPeer]
  );

  return (
    <div className="h-screen bg-zinc-950 text-zinc-400 font-sans flex flex-col overflow-hidden">
      {/* Header */}
      <header className="flex items-center justify-between px-6 py-4 border-b border-zinc-800 shrink-0">
        <div className="flex items-center gap-3">
          <img src="/logo-dark.webp" alt="Repowire" className="w-8 h-8 rounded-lg" />
          <span className="text-white font-bold tracking-tight text-lg">REPOWIRE</span>
        </div>

        <div className="flex items-center gap-4">
          <div
            className={cn(
              "flex items-center gap-2 px-3 py-1 rounded-full text-xs font-medium",
              isConnected ? "text-emerald-500" : "text-red-500"
            )}
          >
            {isConnected ? <Wifi className="w-3.5 h-3.5" /> : <WifiOff className="w-3.5 h-3.5" />}
            <span className="tabular-nums">{onlineCount} peers online</span>
          </div>
          <button
            onClick={refreshData}
            className="p-2 hover:bg-zinc-800 rounded-lg transition-colors"
          >
            <RefreshCw className={cn("w-4 h-4", isRefreshing && "animate-spin")} />
          </button>
        </div>
      </header>

      {/* Body: sidebar + main panel */}
      <div className="flex flex-1 overflow-hidden">
        {/* Sidebar */}
        <aside className="w-56 border-r border-zinc-800 flex flex-col overflow-y-auto shrink-0">
          <div className="px-3 pt-3 pb-2">
            <span className="text-[10px] font-mono text-zinc-600 uppercase tracking-wider">Peers</span>
          </div>
          {sortedPeers.length === 0 ? (
            <p className="text-xs text-zinc-600 px-3 py-2">No peers registered</p>
          ) : (
            <ul className="flex flex-col gap-0.5 px-2 pb-3">
              {sortedPeers.map((peer) => {
                const dotColor =
                  peer.status === "online"
                    ? "bg-emerald-500"
                    : peer.status === "busy"
                    ? "bg-amber-500"
                    : "bg-zinc-600";
                const isSelected = currentSelectedPeer?.name === peer.name;
                return (
                  <li key={peer.name}>
                    <button
                      onClick={() => setSelectedPeer(peer)}
                      className={cn(
                        "w-full flex items-center gap-2.5 px-2 py-2 rounded-md text-left transition-colors",
                        isSelected
                          ? "bg-zinc-800 text-zinc-200"
                          : "hover:bg-zinc-900 text-zinc-400"
                      )}
                    >
                      <span className={cn("w-2 h-2 rounded-full shrink-0", dotColor)} />
                      <span className="text-sm font-medium truncate">{peer.name}</span>
                      {peer.metadata?.branch && (
                        <span className="text-[10px] text-zinc-600 font-mono truncate ml-auto">
                          {String(peer.metadata.branch)}
                        </span>
                      )}
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </aside>

        {/* Main panel */}
        <main className="flex-1 flex flex-col overflow-hidden">
          {/* Tabs */}
          <div className="flex items-center gap-1 px-4 pt-3 pb-0 border-b border-zinc-800 shrink-0">
            {(["chat", "trace"] as const).map((tab) => (
              <button
                key={tab}
                onClick={() => setActiveTab(tab)}
                className={cn(
                  "px-3 py-2 text-xs font-medium rounded-t-md transition-colors border-b-2 -mb-px",
                  activeTab === tab
                    ? "border-zinc-400 text-zinc-200"
                    : "border-transparent text-zinc-500 hover:text-zinc-400"
                )}
              >
                {tab === "chat" ? "Chat" : "Trace"}
              </button>
            ))}
            {activeTab === "trace" && (
              <div className="ml-auto flex items-center gap-2 pb-2">
                <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                <span className="text-xs text-zinc-600">live</span>
              </div>
            )}
          </div>

          {/* Tab content */}
          {activeTab === "chat" ? (
            <div className="flex-1 flex flex-col overflow-hidden">
              <div className="flex-1 overflow-y-auto">
                <ChatPanel peer={currentSelectedPeer} events={events} />
              </div>
              <ComposeBar peers={peers} selectedPeer={currentSelectedPeer} apiBase={API_BASE} />
            </div>
          ) : (
            <div className="flex-1 overflow-y-auto p-4">
              <ActivityFeed events={events} />
            </div>
          )}
        </main>
      </div>
    </div>
  );
}
