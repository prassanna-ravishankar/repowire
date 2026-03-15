"use client";

import React, { useState, useEffect, useCallback, useMemo, useRef } from "react";
import { RefreshCw, Wifi, WifiOff } from "lucide-react";
import { cn } from "./lib/utils";
import { Sidebar } from "./components/Sidebar";
import { OverviewGrid } from "./components/OverviewGrid";
import { PeerHeader } from "./components/PeerHeader";
import { ChatPanel } from "./components/ChatPanel";
import { ComposeBar } from "./components/ComposeBar";
import { ActivityFeed } from "./components/ActivityFeed";
import type { Peer, Event } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeerId, setSelectedPeerId] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<"chat" | "activity">("chat");
  const eventSourceRef = useRef<EventSource | null>(null);

  const fetchPeers = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/peers`);
      if (res.ok) {
        const data = await res.json();
        setPeers(data.peers || data);
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
    eventSourceRef.current = eventSource;

    eventSource.onopen = () => setIsConnected(true);

    eventSource.onmessage = (e) => {
      try {
        const parsed: unknown = JSON.parse(e.data);
        if (
          typeof parsed === "object" &&
          parsed !== null &&
          "id" in parsed &&
          "type" in parsed &&
          "timestamp" in parsed &&
          typeof (parsed as Record<string, unknown>).id === "string" &&
          typeof (parsed as Record<string, unknown>).type === "string" &&
          typeof (parsed as Record<string, unknown>).timestamp === "string"
        ) {
          const event = parsed as Event;
          setEvents((prev) => {
            if (prev.some((existing) => existing.id === event.id)) return prev;
            return [...prev, event];
          });
          // Refresh peers on status changes
          if (event.type === "status_change") fetchPeers();
        }
      } catch (error) {
        console.error("Failed to parse SSE event:", error);
      }
    };

    eventSource.onerror = () => setIsConnected(false);

    const peersInterval = setInterval(fetchPeers, 10000);

    return () => {
      eventSource.close();
      eventSourceRef.current = null;
      clearInterval(peersInterval);
    };
  }, [fetchPeers, fetchEvents]);

  const onlineCount = useMemo(
    () => peers.filter((p) => p.status === "online" || p.status === "busy").length,
    [peers]
  );

  // Resolve selected peer from current data (keeps it fresh as status updates come in)
  const selectedPeer = useMemo(
    () => (selectedPeerId ? peers.find((p) => p.peer_id === selectedPeerId) ?? null : null),
    [peers, selectedPeerId]
  );

  const handleSelectPeer = useCallback((peer: Peer) => {
    setSelectedPeerId(peer.peer_id);
    setActiveTab("chat");
  }, []);

  const handleClosePeer = useCallback(() => {
    setSelectedPeerId(null);
  }, []);

  return (
    <div className="h-screen bg-zinc-950 text-zinc-400 font-sans flex flex-col overflow-hidden">
      {/* Header */}
      <header className="flex items-center justify-between px-6 py-3 border-b border-zinc-800 shrink-0">
        <div className="flex items-center gap-3">
          <img src="/logo-dark.webp" alt="Repowire" className="w-7 h-7 rounded-lg" />
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
            <span>{isConnected ? "Connected" : "Disconnected"}</span>
            <span className="text-zinc-600">·</span>
            <span className="tabular-nums">{onlineCount} online</span>
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
        <Sidebar
          peers={peers}
          selectedPeerId={selectedPeerId}
          onSelectPeer={handleSelectPeer}
        />

        {/* Main panel */}
        <main className="flex-1 flex flex-col overflow-hidden">
          {selectedPeer ? (
            /* State B: Peer Detail */
            <>
              <PeerHeader peer={selectedPeer} onClose={handleClosePeer} />

              {/* Tabs */}
              <div className="flex items-center gap-1 px-4 pt-2 pb-0 border-b border-zinc-800 shrink-0">
                {(["chat", "activity"] as const).map((tab) => (
                  <button
                    key={tab}
                    onClick={() => setActiveTab(tab)}
                    className={cn(
                      "px-3 py-2 text-xs font-medium rounded-t-md transition-colors border-b-2 -mb-px capitalize",
                      activeTab === tab
                        ? "border-zinc-400 text-zinc-200"
                        : "border-transparent text-zinc-500 hover:text-zinc-400"
                    )}
                  >
                    {tab}
                  </button>
                ))}
                {isConnected && (
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
                    <ChatPanel peer={selectedPeer} events={events} />
                  </div>
                  <ComposeBar peer={selectedPeer} apiBase={API_BASE} />
                </div>
              ) : (
                <div className="flex-1 overflow-y-auto p-4">
                  <ActivityFeed events={events} peerFilter={selectedPeer.name} />
                </div>
              )}
            </>
          ) : (
            /* State A: Overview */
            <OverviewGrid
              peers={peers}
              events={events}
              apiBase={API_BASE}
              onSelectPeer={handleSelectPeer}
              onRefresh={refreshData}
            />
          )}
        </main>
      </div>
    </div>
  );
}
