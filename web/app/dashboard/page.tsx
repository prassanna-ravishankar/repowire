"use client";

import React, { useState, useEffect, useCallback, useMemo, useRef } from "react";
import { RefreshCw } from "lucide-react";
import { cn } from "./lib/utils";
import { OverviewGrid } from "./components/OverviewGrid";
import { PeerHeader } from "./components/PeerHeader";
import { ChatPanel } from "./components/ChatPanel";
import { ComposeBar } from "./components/ComposeBar";
import { ActivityFeed } from "./components/ActivityFeed";
import { BottomNav, type NavTab } from "./components/BottomNav";
import { SettingsPanel } from "./components/SettingsPanel";
import type { Peer, Event } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeerId, setSelectedPeerId] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<"chat" | "activity">("chat");
  const [activeNavTab, setActiveNavTab] = useState<NavTab>("dash");
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

  const handleNavTabChange = useCallback((tab: NavTab) => {
    setActiveNavTab(tab);
    setSelectedPeerId(null);
  }, []);

  return (
    <div className="h-dvh bg-surface text-on-surface font-body mesh-bg flex flex-col overflow-hidden">
      {/* Top App Bar */}
      <header className="fixed top-0 left-0 w-full z-50 flex justify-between items-center px-6 h-16 bg-surface">
        <div className="flex items-center gap-3">
          <button onClick={handleClosePeer} className="flex items-center gap-3 hover:opacity-80 transition-opacity">
            <span className="material-symbols-outlined text-cyan-400">hub</span>
            <h1 className="text-xl font-bold tracking-widest text-cyan-400 font-headline uppercase">
              REPOWIRE
            </h1>
          </button>
        </div>

        <div className="flex items-center gap-4">
          <div
            className={cn(
              "flex items-center gap-2 bg-surface-container-low px-3 py-1 rounded shadow-inner",
              isConnected ? "text-secondary" : "text-error"
            )}
          >
            <span className={cn("w-2 h-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
            <span className="text-[10px] font-headline font-bold uppercase tracking-widest">
              {isConnected ? "Mesh Connected" : "Disconnected"}
            </span>
          </div>
          <button
            onClick={refreshData}
            className="w-8 h-8 rounded flex items-center justify-center hover:bg-surface-container-high transition-colors"
          >
            <RefreshCw className={cn("w-4 h-4 text-on-surface-variant", isRefreshing && "animate-spin")} />
          </button>
        </div>
      </header>

      {/* Header separator */}
      <div className="fixed top-16 left-0 w-full z-40 bg-surface-container-low h-[2px]" />

      {/* Main Content */}
      <main className="flex-1 pt-[68px] pb-24 overflow-y-auto">
        {selectedPeer ? (
          /* Peer Detail View */
          <div className="flex flex-col h-full">
            <PeerHeader peer={selectedPeer} onClose={handleClosePeer} />

            {/* Chat/Activity Tabs */}
            <div className="flex items-center gap-1 px-4 pt-2 pb-0 shrink-0">
              {(["chat", "activity"] as const).map((tab) => (
                <button
                  key={tab}
                  onClick={() => setActiveTab(tab)}
                  className={cn(
                    "px-3 py-2 text-[10px] font-headline font-bold uppercase tracking-widest transition-colors border-b-2 -mb-px",
                    activeTab === tab
                      ? "border-primary text-primary"
                      : "border-transparent text-outline hover:text-on-surface-variant"
                  )}
                >
                  {tab}
                </button>
              ))}
              {isConnected && (
                <div className="ml-auto flex items-center gap-2 pb-2">
                  <span className="w-2 h-2 rounded-full bg-secondary pulse-online" />
                  <span className="text-[10px] font-mono text-outline uppercase tracking-widest">live</span>
                </div>
              )}
            </div>

            {activeTab === "chat" ? (
              <div className="flex-1 flex flex-col overflow-hidden">
                <div className="flex-1 overflow-y-auto">
                  <ChatPanel peer={selectedPeer} events={events} />
                </div>
                <ComposeBar key={selectedPeer.peer_id} peer={selectedPeer} apiBase={API_BASE} onSent={refreshData} />
              </div>
            ) : (
              <div className="flex-1 overflow-y-auto px-4 py-4">
                <ActivityFeed events={events} peerFilter={selectedPeer.peer_id} peerName={selectedPeer.name} />
              </div>
            )}
          </div>
        ) : (
          /* Tab Content */
          <>
            {activeNavTab === "dash" && (
              <OverviewGrid
                peers={peers}
                events={events}
                apiBase={API_BASE}
                onSelectPeer={handleSelectPeer}
                onRefresh={refreshData}
              />
            )}
            {activeNavTab === "logs" && (
              <div className="px-4 max-w-2xl mx-auto">
                <ActivityFeed events={events} peers={peers} />
              </div>
            )}
            {activeNavTab === "config" && (
              <SettingsPanel apiBase={API_BASE} isConnected={isConnected} />
            )}
          </>
        )}
      </main>

      {/* Bottom Navigation */}
      <BottomNav activeTab={activeNavTab} onTabChange={handleNavTabChange} />
    </div>
  );
}
